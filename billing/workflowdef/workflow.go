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

	AddLineItemUpdateName = "add-line-item"
	GetTotalQueryName     = "get-total"

	MarkClosedActivityName      = "billing.mark-closed-in-db"
	SendInvoiceActivityName     = "billing.send-invoice-email"
	PersistLineItemActivityName = "billing.persist-line-item"

	defaultTTL = 30 * 24 * time.Hour
)

// ---------------------------------------------------------------------------
// Types exchanged through Temporal (must be JSON-serialisable)
// ---------------------------------------------------------------------------

// BillingWorkflowInput is the initial input when starting a billing workflow.
type BillingWorkflowInput struct {
	BillID    string    `json:"bill_id"`
	PeriodEnd time.Time `json:"period_end"`
}

// AddLineItemUpdateInput is the payload sent to the running workflow via Update.
type AddLineItemUpdateInput struct {
	Description    string `json:"description"`
	AmountMinor    int64  `json:"amount_minor"`
	IdempotencyKey string `json:"idempotency_key"`
	PayloadHash    string `json:"payload_hash"`
}

// PersistLineItemInput is the payload sent to the PersistLineItem activity.
type PersistLineItemInput struct {
	BillID         string `json:"bill_id"`
	Description    string `json:"description"`
	AmountMinor    int64  `json:"amount_minor"`
	IdempotencyKey string `json:"idempotency_key"`
	PayloadHash    string `json:"payload_hash"`
}

// PersistLineItemResult is returned by the PersistLineItem activity and
// propagated back through the Update to the API caller.
type PersistLineItemResult struct {
	Inserted bool       `json:"inserted"`
	Item     WfLineItem `json:"item"`
	Bill     WfBill     `json:"bill"`
}

// WfLineItem is the Temporal-serialisable representation of a line item.
type WfLineItem struct {
	ID             string    `json:"id"`
	BillID         string    `json:"bill_id"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	Description    string    `json:"description"`
	AmountMinor    int64     `json:"amount_minor"`
	CreatedAt      time.Time `json:"created_at"`
}

// WfBill is the Temporal-serialisable representation of a bill.
type WfBill struct {
	ID          string     `json:"id"`
	AccountID   string     `json:"account_id"`
	Currency    string     `json:"currency"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PeriodStart time.Time  `json:"period_start"`
	PeriodEnd   time.Time  `json:"period_end"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	TotalMinor  int64      `json:"total_minor"`
}

// ---------------------------------------------------------------------------
// Workflow
// ---------------------------------------------------------------------------

// BillingWorkflow owns the full lifecycle of a bill.
//
//   - Registers an Update handler ("add-line-item") so callers can add line
//     items synchronously through Temporal, with durable retry on the
//     underlying DB write.
//   - Registers a Query handler ("get-total") for a fast running-total check.
//   - Waits for period-end timer OR a manual close signal.
//   - On exit, marks the bill closed and sends the invoice via activities.
func BillingWorkflow(ctx workflow.Context, in BillingWorkflowInput) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout:    30 * time.Second,
		ScheduleToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	}

	var totalMinor int64
	billOpen := true

	// --- Query handler: running total without a DB hit ---
	if err := workflow.SetQueryHandler(ctx, GetTotalQueryName, func() (int64, error) {
		return totalMinor, nil
	}); err != nil {
		return err
	}

	// --- Update handler: add-line-item (synchronous) ---
	if err := workflow.SetUpdateHandlerWithOptions(
		ctx,
		AddLineItemUpdateName,
		func(ctx workflow.Context, req AddLineItemUpdateInput) (*PersistLineItemResult, error) {
			actCtx := workflow.WithActivityOptions(ctx, ao)
			var result PersistLineItemResult
			err := workflow.ExecuteActivity(actCtx, PersistLineItemActivityName, PersistLineItemInput{
				BillID:         in.BillID,
				Description:    req.Description,
				AmountMinor:    req.AmountMinor,
				IdempotencyKey: req.IdempotencyKey,
				PayloadHash:    req.PayloadHash,
			}).Get(ctx, &result)
			if err != nil {
				return nil, err
			}
			if result.Inserted {
				totalMinor += req.AmountMinor
			}
			return &result, nil
		},
		workflow.UpdateHandlerOptions{
			Validator: func(ctx workflow.Context, req AddLineItemUpdateInput) error {
				if !billOpen {
					return temporal.NewApplicationError(
						"bill is not open",
						"FAILED_PRECONDITION",
						nil,
						true,
					)
				}
				return nil
			},
		},
	); err != nil {
		return err
	}

	// --- Timer for automatic close at period end ---
	wait := defaultTTL
	if !in.PeriodEnd.IsZero() {
		wait = in.PeriodEnd.Sub(workflow.Now(ctx))
		if wait < 0 {
			wait = 0
		}
	}
	timer := workflow.NewTimer(ctx, wait)
	closeSigCh := workflow.GetSignalChannel(ctx, CloseSignalName)

	// --- Accrual loop ---
	// The selector blocks until the timer fires or a close signal arrives.
	// Update handlers are serviced by Temporal while the selector is blocked,
	// so line items continue to be accepted during this period.
	for billOpen {
		sel := workflow.NewSelector(ctx)
		sel.AddFuture(timer, func(workflow.Future) {
			billOpen = false
		})
		sel.AddReceive(closeSigCh, func(ch workflow.ReceiveChannel, more bool) {
			ch.Receive(ctx, nil)
			billOpen = false
		})
		sel.Select(ctx)
	}

	// --- Close phase: mark closed + send invoice ---
	actCtx := workflow.WithActivityOptions(ctx, ao)
	if err := workflow.ExecuteActivity(actCtx, MarkClosedActivityName, in.BillID).Get(ctx, nil); err != nil {
		return err
	}
	return workflow.ExecuteActivity(actCtx, SendInvoiceActivityName, in.BillID).Get(ctx, nil)
}
