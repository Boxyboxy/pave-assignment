package workflowdef

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	BillingTaskQueue = "billing-workflow"
	WorkflowName     = "billing-workflow-v1"
	CloseSignalName  = "close-bill"

	MarkClosedActivityName  = "billing.mark-closed-in-db"
	SendInvoiceActivityName = "billing.send-invoice-email"

	defaultTTL = 30 * 24 * time.Hour
)

type BillingWorkflowInput struct {
	BillID    string    `json:"bill_id"`
	PeriodEnd time.Time `json:"period_end"`
}

// BillingWorkflow waits until period end (defaulting to 30 days) or until manual close signal.
// It then marks the bill closed and triggers invoice notification.
func BillingWorkflow(ctx workflow.Context, in BillingWorkflowInput) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    30 * time.Second,
		ScheduleToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	})

	wait := defaultTTL
	if !in.PeriodEnd.IsZero() {
		wait = in.PeriodEnd.Sub(workflow.Now(ctx))
		if wait < 0 {
			wait = 0
		}
	}
	timer := workflow.NewTimer(ctx, wait)
	closeSignals := workflow.GetSignalChannel(ctx, CloseSignalName)

	selector := workflow.NewSelector(ctx)
	selector.AddFuture(timer, func(workflow.Future) {})
	selector.AddReceive(closeSignals, func(workflow.ReceiveChannel, bool) {})
	selector.Select(ctx)

	if err := workflow.ExecuteActivity(ctx, MarkClosedActivityName, in.BillID).Get(ctx, nil); err != nil {
		return err
	}
	return workflow.ExecuteActivity(ctx, SendInvoiceActivityName, in.BillID).Get(ctx, nil)
}
