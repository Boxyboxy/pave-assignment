package workflowdef

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// ---------------------------------------------------------------------------
// Callback types — injected by the worker at startup
// ---------------------------------------------------------------------------

// CloseBillFunc marks a bill as closed in the database.
type CloseBillFunc func(ctx context.Context, billID string) error

// SendInvoiceFunc sends (or enqueues) the invoice email for a closed bill.
type SendInvoiceFunc func(ctx context.Context, billID string) error

// PersistLineItemFunc idempotently persists a line item and updates the bill total.
type PersistLineItemFunc func(ctx context.Context, input PersistLineItemInput) (*PersistLineItemResult, error)

// ---------------------------------------------------------------------------
// Activities struct (hosts all Temporal activity implementations)
// ---------------------------------------------------------------------------

// Activities groups all Temporal activity implementations behind injected callbacks.
type Activities struct {
	closeBill       CloseBillFunc
	sendInvoice     SendInvoiceFunc
	persistLineItem PersistLineItemFunc
}

// NewActivities constructs an Activities instance with the given callback implementations.
func NewActivities(closeBill CloseBillFunc, sendInvoice SendInvoiceFunc, persistLineItem PersistLineItemFunc) *Activities {
	return &Activities{
		closeBill:       closeBill,
		sendInvoice:     sendInvoice,
		persistLineItem: persistLineItem,
	}
}

// ---------------------------------------------------------------------------
// Activity: MarkClosedInDB wrapper
// ---------------------------------------------------------------------------

// MarkClosedInDB transitions a bill to CLOSED via the injected CloseBillFunc.
func (a *Activities) MarkClosedInDB(ctx context.Context, billID string) error {
	if a.closeBill == nil {
		return fmt.Errorf("closeBill callback is not configured")
	}
	if err := a.closeBill(ctx, billID); err != nil {
		return err
	}
	logActivityInfo(ctx, "bill closed in db", "bill_id", billID)
	return nil
}

// ---------------------------------------------------------------------------
// Activity: SendInvoiceEmail wrapper
// ---------------------------------------------------------------------------

// SendInvoiceEmail sends the invoice via the injected SendInvoiceFunc.
func (a *Activities) SendInvoiceEmail(ctx context.Context, billID string) error {
	if a.sendInvoice == nil {
		return fmt.Errorf("sendInvoice callback is not configured")
	}
	if err := a.sendInvoice(ctx, billID); err != nil {
		return err
	}
	logActivityInfo(ctx, "invoice email queued", "bill_id", billID)
	return nil
}

// ---------------------------------------------------------------------------
// Activity: PersistLineItem wrapper
// ---------------------------------------------------------------------------

// PersistLineItem delegates to the injected PersistLineItemFunc and maps
// domain errors to non-retryable Temporal ApplicationErrors.
func (a *Activities) PersistLineItem(ctx context.Context, input PersistLineItemInput) (*PersistLineItemResult, error) {
	if a.persistLineItem == nil {
		return nil, fmt.Errorf("persistLineItem callback is not configured")
	}
	result, err := a.persistLineItem(ctx, input)
	if err != nil {
		// Convert known domain errors to non-retryable Temporal application
		// errors so Temporal does not retry business-logic failures.
		if errors.Is(err, ErrBillNotFound) {
			return nil, temporal.NewApplicationError(err.Error(), "NOT_FOUND", err, true)
		}
		if errors.Is(err, ErrBillNotOpen) {
			return nil, temporal.NewApplicationError(err.Error(), "FAILED_PRECONDITION", err, true)
		}
		if errors.Is(err, ErrPayloadMismatch) {
			return nil, temporal.NewApplicationError(err.Error(), "ALREADY_EXISTS", err, true)
		}
		// All other errors are retryable (transient DB issues, etc.).
		return nil, err
	}
	logActivityInfo(ctx, "line item persisted", "bill_id", input.BillID, "idempotency_key", input.IdempotencyKey)
	return result, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// logActivityInfo emits a structured log via the Temporal activity logger.
// It recovers gracefully when called outside a real activity context (e.g. unit tests).
func logActivityInfo(ctx context.Context, msg string, keyvals ...any) {
	defer func() { _ = recover() }()
	activity.GetLogger(ctx).Info(msg, keyvals...)
}
