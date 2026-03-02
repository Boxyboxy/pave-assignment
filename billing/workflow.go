package billing

import (
	"context"
	"fmt"
	"log"
	"os"

	"pave-assignment/billing/workflowdef"

	"encore.dev/beta/errs"
	"go.temporal.io/sdk/client"
)

// dialTemporal creates a new Temporal client connection.
//
// The address is read from TEMPORAL_ADDRESS (required in non-dev environments).
// If the variable is unset, we fall back to localhost:7233 which is the default
// address used by `temporal server start-dev` for local development.
func dialTemporal() (client.Client, error) {
	namespace := os.Getenv("TEMPORAL_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}
	hostPort := os.Getenv("TEMPORAL_ADDRESS")
	if hostPort == "" {
		hostPort = "127.0.0.1:7233"
		log.Printf("TEMPORAL_ADDRESS not set, falling back to %s (local dev default)", hostPort)
	}

	c, err := client.Dial(client.Options{
		HostPort:  hostPort,
		Namespace: namespace,
	})
	if err != nil {
		return nil, errs.Wrap(err, fmt.Sprintf("dial temporal (address=%s, namespace=%s)", hostPort, namespace))
	}
	log.Printf("temporal client connected address=%s namespace=%s", hostPort, namespace)
	return c, nil
}

// getTemporalClient returns a lazily-initialised Temporal client held by
// the Service instance. This avoids global mutable state and keeps the
// client lifecycle tied to the service.
func (s *Service) getTemporalClient() (client.Client, error) {
	s.tcMu.Lock()
	defer s.tcMu.Unlock()

	if s.tc != nil {
		return s.tc, nil
	}

	tc, err := dialTemporal()
	if err != nil {
		return nil, err
	}
	s.tc = tc
	return s.tc, nil
}

func (s *Service) startBillingWorkflow(ctx context.Context, workflowID string, in workflowdef.BillingWorkflowInput) (string, error) {
	c, err := s.getTemporalClient()
	if err != nil {
		return "", err
	}
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: workflowdef.BillingTaskQueue,
	}, workflowdef.WorkflowName, in)
	if err != nil {
		return "", errs.Wrap(err, "start billing workflow")
	}
	return run.GetRunID(), nil
}

func (s *Service) signalCloseBillingWorkflow(ctx context.Context, workflowID string) error {
	c, err := s.getTemporalClient()
	if err != nil {
		return err
	}
	if err := c.SignalWorkflow(ctx, workflowID, "", workflowdef.CloseSignalName, struct{}{}); err != nil {
		return errs.Wrap(err, "signal close billing workflow")
	}
	return nil
}

func (s *Service) terminateBillingWorkflow(ctx context.Context, workflowID, runID, reason string) error {
	c, err := s.getTemporalClient()
	if err != nil {
		return err
	}
	if err := c.TerminateWorkflow(ctx, workflowID, runID, reason); err != nil {
		return errs.Wrap(err, "terminate billing workflow")
	}
	return nil
}

func (s *Service) describeBillingWorkflowRunID(ctx context.Context, workflowID string) (string, error) {
	c, err := s.getTemporalClient()
	if err != nil {
		return "", err
	}
	desc, err := c.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		return "", errs.Wrap(err, "describe billing workflow")
	}
	info := desc.GetWorkflowExecutionInfo()
	if info == nil {
		return "", errs.B().Code(errs.Internal).Msg("missing workflow execution info").Err()
	}
	exec := info.GetExecution()
	if exec == nil {
		return "", errs.B().Code(errs.Internal).Msg("missing workflow execution handle").Err()
	}
	runID := exec.GetRunId()
	if runID == "" {
		return "", errs.B().Code(errs.Internal).Msg(fmt.Sprintf("workflow %s returned empty run id", workflowID)).Err()
	}
	return runID, nil
}
