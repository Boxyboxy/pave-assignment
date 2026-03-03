package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"pave-assignment/billing/workflowdef"
	"pave-assignment/internal/workerdb"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// pgLineItemPersister uses sqlc-generated queries for line-item persistence.
// ---------------------------------------------------------------------------

// pgLineItemPersister implements PersistLineItemFunc using sqlc-generated queries.
type pgLineItemPersister struct {
	db *sql.DB
}

// PersistLineItem idempotently inserts a line item and updates the bill total
// inside a single transaction with SELECT … FOR UPDATE row locking.
func (p *pgLineItemPersister) PersistLineItem(ctx context.Context, input workflowdef.PersistLineItemInput) (*workflowdef.PersistLineItemResult, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	billUUID, err := uuid.Parse(input.BillID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid bill id %q", workflowdef.ErrBillNotFound, input.BillID)
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := workerdb.New(tx)

	// Lock bill row and verify status.
	bill, err := q.LockBillForUpdate(ctx, billUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", workflowdef.ErrBillNotFound, input.BillID)
		}
		return nil, fmt.Errorf("lock bill: %w", err)
	}
	if bill.Status != "OPEN" {
		return nil, fmt.Errorf("%w: bill is %s", workflowdef.ErrBillNotOpen, bill.Status)
	}

	// Attempt idempotent insert.
	itemID := uuid.New()
	inserted := false
	var itemCreatedAt time.Time

	insertResult, err := q.InsertLineItemIdempotent(ctx, workerdb.InsertLineItemIdempotentParams{
		ID:             itemID,
		BillID:         billUUID,
		IdempotencyKey: input.IdempotencyKey,
		PayloadHash:    input.PayloadHash,
		Description:    input.Description,
		AmountMinor:    input.AmountMinor,
	})

	if err == nil {
		// Fresh insert succeeded.
		inserted = true
		itemID = insertResult.ID
		itemCreatedAt = insertResult.CreatedAt
	} else if errors.Is(err, sql.ErrNoRows) {
		// Conflict — load existing item and verify payload hash.
		existing, err := q.GetLineItemByBillAndKey(ctx, workerdb.GetLineItemByBillAndKeyParams{
			BillID:         billUUID,
			IdempotencyKey: input.IdempotencyKey,
		})
		if err != nil {
			return nil, fmt.Errorf("load existing line item: %w", err)
		}
		if existing.PayloadHash != input.PayloadHash {
			return nil, fmt.Errorf(
				"%w: idempotency_key already used with different payload",
				workflowdef.ErrPayloadMismatch,
			)
		}
		itemID = existing.ID
		input.Description = existing.Description
		input.AmountMinor = existing.AmountMinor
		itemCreatedAt = existing.CreatedAt
	} else {
		return nil, fmt.Errorf("insert line item: %w", err)
	}

	// Update bill running total on fresh insert.
	totalMinor := bill.TotalMinor
	updatedAt := bill.UpdatedAt
	if inserted {
		updated, err := q.IncrementBillTotal(ctx, workerdb.IncrementBillTotalParams{
			Amount: input.AmountMinor,
			BillID: billUUID,
		})
		if err != nil {
			return nil, fmt.Errorf("update bill total: %w", err)
		}
		totalMinor = updated.TotalMinor
		updatedAt = updated.UpdatedAt
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	var closedAt *time.Time
	if bill.ClosedAt.Valid {
		closedAt = &bill.ClosedAt.Time
	}
	return &workflowdef.PersistLineItemResult{
		Inserted: inserted,
		Item: workflowdef.WfLineItem{
			ID:             itemID.String(),
			BillID:         input.BillID,
			IdempotencyKey: input.IdempotencyKey,
			Description:    input.Description,
			AmountMinor:    input.AmountMinor,
			CreatedAt:      itemCreatedAt,
		},
		Bill: workflowdef.WfBill{
			ID:          input.BillID,
			AccountID:   bill.AccountID,
			Currency:    bill.Currency,
			Status:      bill.Status,
			CreatedAt:   bill.CreatedAt,
			UpdatedAt:   updatedAt,
			PeriodStart: bill.PeriodStart,
			PeriodEnd:   bill.PeriodEnd,
			ClosedAt:    closedAt,
			TotalMinor:  totalMinor,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------------

// sendInvoiceEmailStub is a no-op placeholder for the invoice email activity.
func sendInvoiceEmailStub(_ context.Context, _ string) error {
	return nil
}
