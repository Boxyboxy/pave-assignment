package billing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"pave-assignment/billing/workflowdef"

	"encore.dev/beta/errs"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
)

// ---------------------------------------------------------------------------
// Service definition
// ---------------------------------------------------------------------------

// Service is the Encore billing service. It holds a lazily-initialised
// Temporal client used by all workflow operations.
//
//encore:service
type Service struct {
	tc   client.Client // access via getTemporalClient()
	tcMu sync.Mutex
}

// initService is the Encore service constructor; Temporal connection is deferred to first use.
func initService() (*Service, error) {
	return &Service{}, nil
}

// ---------------------------------------------------------------------------
// Workflow ID conventions
// ---------------------------------------------------------------------------

// billingWorkflowID returns the deterministic Temporal workflow ID for a bill.
func billingWorkflowID(billID uuid.UUID) string {
	return "bill-" + billID.String()
}

// fallbackCloseWorkflowID returns the deterministic Temporal workflow ID used
// when the primary workflow is unreachable and a fallback close is needed.
func fallbackCloseWorkflowID(billID uuid.UUID) string {
	return "bill-close-fallback-" + billID.String()
}

// closeWaitTimeout is the maximum time the API will block waiting for a
// close-bill workflow to finish before returning an "in progress" response.
const closeWaitTimeout = 2 * time.Second

// ---------------------------------------------------------------------------
// Temporal client lifecycle
// ---------------------------------------------------------------------------

// dialTemporal creates a new Temporal client connection.
//
// The address is read from TEMPORAL_ADDRESS (required in non-dev environments).
// If the variable is unset, we fall back to 127.0.0.1:7233 which is the default
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

// ---------------------------------------------------------------------------
// Temporal workflow operations
// ---------------------------------------------------------------------------

// startBillingWorkflow starts a new BillingWorkflow and returns its run ID.
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

// signalCloseBillingWorkflow sends the close signal to a running billing workflow.
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

// terminateBillingWorkflow forcefully terminates a workflow execution.
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

// updateLineItemWorkflow sends an add-line-item Update to the workflow and
// blocks until the activity completes, returning the persisted result.
func (s *Service) updateLineItemWorkflow(ctx context.Context, workflowID string, input workflowdef.AddLineItemUpdateInput) (*workflowdef.PersistLineItemResult, error) {
	c, err := s.getTemporalClient()
	if err != nil {
		return nil, err
	}
	handle, err := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   workflowID,
		UpdateName:   workflowdef.AddLineItemUpdateName,
		Args:         []interface{}{input},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return nil, err
	}
	var result workflowdef.PersistLineItemResult
	if err := handle.Get(ctx, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// waitForWorkflow blocks until the given workflow run completes.
func (s *Service) waitForWorkflow(ctx context.Context, workflowID string) error {
	c, err := s.getTemporalClient()
	if err != nil {
		return err
	}
	return c.GetWorkflow(ctx, workflowID, "").Get(ctx, nil)
}

// waitForWorkflowWithTimeout waits for the given workflow to complete, but
// enforces an upper bound on how long the caller is blocked. If the timeout
// elapses before completion, an Unavailable error is returned so callers can
// retry or treat the bill as "processing" instead of assuming it is closed.
func (s *Service) waitForWorkflowWithTimeout(parentCtx context.Context, workflowID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	if err := s.waitForWorkflow(ctx, workflowID); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return errs.B().
				Code(errs.Unavailable).
				Msg("bill close is still processing; please retry shortly").
				Err()
		}
		return errs.B().
			Code(errs.Internal).
			Msg(fmt.Sprintf("waiting for bill close workflow failed: %v", err)).
			Err()
	}
	return nil
}

// describeBillingWorkflowRunID fetches the current run ID for a workflow via Describe.
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
