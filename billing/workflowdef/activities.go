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

type CloseBillFunc func(ctx context.Context, billID string) error
type SendInvoiceFunc func(ctx context.Context, billID string) error
type PersistLineItemFunc func(ctx context.Context, input PersistLineItemInput) (*PersistLineItemResult, error)

// ---------------------------------------------------------------------------
// Activities struct (hosts all Temporal activity implementations)
// ---------------------------------------------------------------------------

type Activities struct {
	closeBill       CloseBillFunc
	sendInvoice     SendInvoiceFunc
	persistLineItem PersistLineItemFunc
}

func NewActivities(closeBill CloseBillFunc, sendInvoice SendInvoiceFunc, persistLineItem PersistLineItemFunc) *Activities {
	return &Activities{
		closeBill:       closeBill,
		sendInvoice:     sendInvoice,
		persistLineItem: persistLineItem,
	}
}

// ---------------------------------------------------------------------------
// Activity: MarkClosedInDB
// ---------------------------------------------------------------------------

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
// Activity: SendInvoiceEmail
// ---------------------------------------------------------------------------

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
// Activity: PersistLineItem
// ---------------------------------------------------------------------------

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

func logActivityInfo(ctx context.Context, msg string, keyvals ...any) {
	// In regular activity execution, Temporal injects activity context with logger.
	// Some unit tests call methods directly with context.Background(); avoid panicking there.
	defer func() { _ = recover() }()
	activity.GetLogger(ctx).Info(msg, keyvals...)
}
