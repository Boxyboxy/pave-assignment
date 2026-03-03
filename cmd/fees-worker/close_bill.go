package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"pave-assignment/billing/workflowdef"
	"pave-assignment/internal/workerdb"

	"github.com/google/uuid"
)

// closeBillFunc returns a CloseBillFunc that uses sqlc-generated queries.
func closeBillFunc(db *sql.DB) workflowdef.CloseBillFunc {
	return func(ctx context.Context, billID string) error {
		ctx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()

		billUUID, err := uuid.Parse(billID)
		if err != nil {
			return fmt.Errorf("%w: invalid bill id %q", workflowdef.ErrBillNotFound, billID)
		}

		q := workerdb.New(db)

		result, err := q.CloseBillByID(ctx, billUUID)
		if err != nil {
			return fmt.Errorf("close bill %s: %w", billID, err)
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			// Bill was not OPEN — verify it exists.
			status, err := q.GetBillStatus(ctx, billUUID)
			if err != nil {
				return fmt.Errorf("%w: %s", workflowdef.ErrBillNotFound, billID)
			}
			log.Printf("close bill %s was no-op, current status=%s", billID, status)
		}
		return nil
	}
}
