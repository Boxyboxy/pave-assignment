package workflowdef

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/activity"
)

type CloseBillFunc func(ctx context.Context, billID string) error
type SendInvoiceFunc func(ctx context.Context, billID string) error

type Activities struct {
	closeBill   CloseBillFunc
	sendInvoice SendInvoiceFunc
}

func NewActivities(closeBill CloseBillFunc, sendInvoice SendInvoiceFunc) *Activities {
	return &Activities{
		closeBill:   closeBill,
		sendInvoice: sendInvoice,
	}
}

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

func logActivityInfo(ctx context.Context, msg string, keyvals ...any) {
	// In regular activity execution, Temporal injects activity context with logger.
	// Some unit tests call methods directly with context.Background(); avoid panicking there.
	defer func() { _ = recover() }()
	activity.GetLogger(ctx).Info(msg, keyvals...)
}
