package billing

import (
	"errors"
	"fmt"

	"pave-assignment/billing/workflowdef"

	"encore.dev/beta/errs"
	"go.temporal.io/sdk/temporal"
)

// ---------------------------------------------------------------------------
// Workflow ↔ API type conversion
// ---------------------------------------------------------------------------

// wfBillToBill converts a Temporal-serialisable bill into an API-facing Bill.
func wfBillToBill(b workflowdef.WfBill) Bill {
	return Bill{
		ID:          b.ID,
		AccountID:   b.AccountID,
		Currency:    Currency(b.Currency),
		Status:      BillStatus(b.Status),
		CreatedAt:   b.CreatedAt,
		UpdatedAt:   b.UpdatedAt,
		PeriodStart: b.PeriodStart,
		PeriodEnd:   b.PeriodEnd,
		ClosedAt:    b.ClosedAt,
		TotalMinor:  MinorUnit(b.TotalMinor),
	}
}

// wfLineItemToLineItem converts a Temporal-serialisable line item into an API-facing LineItem.
func wfLineItemToLineItem(li workflowdef.WfLineItem) LineItem {
	return LineItem{
		ID:             li.ID,
		BillID:         li.BillID,
		IdempotencyKey: li.IdempotencyKey,
		Description:    li.Description,
		AmountMinor:    MinorUnit(li.AmountMinor),
		CreatedAt:      li.CreatedAt,
	}
}

// ---------------------------------------------------------------------------
// Temporal error mapping
// ---------------------------------------------------------------------------

// mapTemporalError converts Temporal-domain errors into Encore API errors
// with the appropriate gRPC status code.
func mapTemporalError(err error) error {
	if err == nil {
		return nil
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		switch appErr.Type() {
		case "FAILED_PRECONDITION":
			return errs.B().Code(errs.FailedPrecondition).Msg(appErr.Message()).Err()
		case "ALREADY_EXISTS":
			return errs.B().Code(errs.AlreadyExists).Msg(appErr.Message()).Err()
		case "NOT_FOUND":
			return errs.B().Code(errs.NotFound).Msg(appErr.Message()).Err()
		}
	}
	// For unexpected errors, propagate a trimmed version of the original
	// message so callers have some context without leaking excessive detail.
	return errs.B().
		Code(errs.Internal).
		Msg(fmt.Sprintf("workflow operation failed: %v", err)).
		Err()
}
