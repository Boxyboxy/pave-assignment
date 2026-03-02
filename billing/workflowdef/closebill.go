package workflowdef

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// CloseBillSQL is the idempotent UPDATE that transitions a bill from OPEN to CLOSED.
// It is the single source of truth for the close mutation used by the Temporal worker.
// The API service uses its own transactional close path with row locking for tighter
// serialization with concurrent addLineItem transactions.
const CloseBillSQL = `
	UPDATE bills SET status = 'CLOSED', closed_at = COALESCE(closed_at, NOW()), updated_at = NOW()
	WHERE id = $1 AND status = 'OPEN'`

// BillStatusSQL returns the current status of a bill by ID.
const BillStatusSQL = `SELECT status FROM bills WHERE id = $1`

// ErrBillNotFound is returned when a close is attempted on a non-existent bill.
var ErrBillNotFound = errors.New("bill not found")

// BillCloser abstracts the DB operations needed by CloseBillInDB.
// Both encore's sqldb.Database and stdlib database/sql can implement this
// via thin adapters, eliminating duplicated close logic across processes.
type BillCloser interface {
	// ExecCloseBill attempts to transition the bill from OPEN to CLOSED.
	// Returns the number of rows affected (0 if already closed/cancelled, 1 if transitioned).
	ExecCloseBill(ctx context.Context, billID string) (rowsAffected int64, err error)

	// GetBillStatus returns the current status string for a bill.
	// Returns an error wrapping sql.ErrNoRows (or equivalent) if the bill does not exist.
	GetBillStatus(ctx context.Context, billID string) (status string, err error)
}

// CloseBillInDB executes the idempotent close-bill logic through the provided BillCloser.
//
// If the bill is OPEN it transitions to CLOSED. If it is already CLOSED or
// CANCELLED the call succeeds as a no-op (idempotent). If the bill does not
// exist, an error wrapping ErrBillNotFound is returned.
func CloseBillInDB(ctx context.Context, billID string, closer BillCloser) error {
	rowsAffected, err := closer.ExecCloseBill(ctx, billID)
	if err != nil {
		return fmt.Errorf("close bill %s: %w", billID, err)
	}
	if rowsAffected == 0 {
		// Bill was not OPEN — verify it exists and log the current state.
		status, err := closer.GetBillStatus(ctx, billID)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrBillNotFound, billID)
		}
		log.Printf("close bill %s was no-op, current status=%s", billID, status)
	}
	return nil
}
