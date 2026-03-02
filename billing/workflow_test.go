package billing

import (
	"context"
	"pave-assignment/billing/workflowdef"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

func TestBillingWorkflow_ClosesOnSignal(t *testing.T) {
	t.Parallel()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowdef.BillingWorkflow)
	billID := "8cbf374a-7b61-4d03-8ff8-e46265d66abc"
	closedCalls := 0
	invoiceCalls := 0

	env.RegisterActivityWithOptions(func(context.Context, string) error {
		closedCalls++
		return nil
	}, activity.RegisterOptions{Name: workflowdef.MarkClosedActivityName})
	env.RegisterActivityWithOptions(func(context.Context, string) error {
		invoiceCalls++
		return nil
	}, activity.RegisterOptions{Name: workflowdef.SendInvoiceActivityName})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflowdef.CloseSignalName, struct{}{})
	}, time.Second)

	env.ExecuteWorkflow(workflowdef.BillingWorkflow, workflowdef.BillingWorkflowInput{
		BillID:    billID,
		PeriodEnd: time.Now().Add(24 * time.Hour),
	})
	if !env.IsWorkflowCompleted() {
		t.Fatalf("workflow not completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow returned error: %v", err)
	}
	if closedCalls != 1 || invoiceCalls != 1 {
		t.Fatalf("expected one close and one invoice activity call, got close=%d invoice=%d", closedCalls, invoiceCalls)
	}
}

func TestBillingWorkflow_ClosesOnTimer(t *testing.T) {
	t.Parallel()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowdef.BillingWorkflow)
	billID := "8cbf374a-7b61-4d03-8ff8-e46265d66abc"
	closedCalls := 0
	invoiceCalls := 0

	env.RegisterActivityWithOptions(func(context.Context, string) error {
		closedCalls++
		return nil
	}, activity.RegisterOptions{Name: workflowdef.MarkClosedActivityName})
	env.RegisterActivityWithOptions(func(context.Context, string) error {
		invoiceCalls++
		return nil
	}, activity.RegisterOptions{Name: workflowdef.SendInvoiceActivityName})
	env.ExecuteWorkflow(workflowdef.BillingWorkflow, workflowdef.BillingWorkflowInput{
		BillID:    billID,
		PeriodEnd: time.Now().Add(time.Second),
	})
	if !env.IsWorkflowCompleted() {
		t.Fatalf("workflow not completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow returned error: %v", err)
	}
	if closedCalls != 1 || invoiceCalls != 1 {
		t.Fatalf("expected one close and one invoice activity call, got close=%d invoice=%d", closedCalls, invoiceCalls)
	}
}
