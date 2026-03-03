package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"pave-assignment/billing/workflowdef"
	"pave-assignment/internal/workerdb"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// queryTimeout is the maximum duration for a single DB query or transaction.
const queryTimeout = 10 * time.Second

func main() {
	hostPort := os.Getenv("TEMPORAL_ADDRESS")
	if hostPort == "" {
		hostPort = "localhost:7233"
	}
	namespace := os.Getenv("TEMPORAL_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable is required (e.g. run: encore db conn-uri billing)")
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("unable to open database: %v", err)
	}
	defer db.Close()

	// Connection pool tuning — prevent unbounded connection growth.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		log.Fatalf("unable to ping database: %v", err)
	}
	log.Println("database connection established")

	var c client.Client
	for attempt := 1; attempt <= 12; attempt++ {
		c, err = client.Dial(client.Options{
			HostPort:  hostPort,
			Namespace: namespace,
		})
		if err == nil {
			break
		}
		log.Printf("temporal dial attempt %d failed: %v", attempt, err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("unable to create temporal client after retries: %v", err)
	}
	defer c.Close()

	persister := &pgLineItemPersister{db: db}

	activities := workflowdef.NewActivities(
		closeBillFunc(db),
		sendInvoiceEmailStub,
		persister.PersistLineItem,
	)

	w := worker.New(c, workflowdef.BillingTaskQueue, worker.Options{})
	w.RegisterWorkflowWithOptions(workflowdef.BillingWorkflow, workflow.RegisterOptions{
		Name: workflowdef.WorkflowName,
	})
	w.RegisterActivityWithOptions(activities.MarkClosedInDB, activity.RegisterOptions{
		Name: workflowdef.MarkClosedActivityName,
	})
	w.RegisterActivityWithOptions(activities.SendInvoiceEmail, activity.RegisterOptions{
		Name: workflowdef.SendInvoiceActivityName,
	})
	w.RegisterActivityWithOptions(activities.PersistLineItem, activity.RegisterOptions{
		Name: workflowdef.PersistLineItemActivityName,
	})

	log.Println("starting billing worker")
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("unable to run worker: %v", err)
	}
}

// ---------------------------------------------------------------------------
// closeBillFunc returns a CloseBillFunc that uses sqlc-generated queries.
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// pgLineItemPersister uses sqlc-generated queries for line-item persistence.
// ---------------------------------------------------------------------------

type pgLineItemPersister struct {
	db *sql.DB
}

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

func sendInvoiceEmailStub(_ context.Context, _ string) error {
	// Stub activity for this assignment.
	return nil
}
