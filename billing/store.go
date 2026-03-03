package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"encore.dev/beta/errs"
	"encore.dev/storage/sqldb"
	"github.com/google/uuid"
)

// billingDB is the Encore-managed database handle for the billing service.
var billingDB = sqldb.NewDatabase("billing", sqldb.DatabaseConfig{
	Migrations: "./migrations",
})

// ---------------------------------------------------------------------------
// Bill columns constant (used by all bill queries)
// ---------------------------------------------------------------------------

// billColumns defines the canonical SELECT column list for bills.
// IMPORTANT: scanBill below MUST match this column order exactly.
const billColumns = "id, account_id, currency, status, created_at, updated_at, period_start, period_end, closed_at, total_minor"

// scanBill scans a row that was selected with billColumns into a Bill struct.
// Keep column order in sync with billColumns above.
func scanBill(row interface{ Scan(dest ...any) error }) (*Bill, error) {
	var (
		id          uuid.UUID
		accountID   string
		currency    string
		status      string
		createdAt   time.Time
		updatedAt   time.Time
		periodStart time.Time
		periodEnd   time.Time
		closedAt    sql.NullTime
		totalMinor  int64
	)
	if err := row.Scan(&id, &accountID, &currency, &status, &createdAt, &updatedAt, &periodStart, &periodEnd, &closedAt, &totalMinor); err != nil {
		return nil, err
	}
	var closedAtPtr *time.Time
	if closedAt.Valid {
		closedAtPtr = &closedAt.Time
	}
	return &Bill{
		ID:          id.String(),
		AccountID:   accountID,
		Currency:    Currency(currency),
		Status:      BillStatus(status),
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		ClosedAt:    closedAtPtr,
		TotalMinor:  MinorUnit(totalMinor),
	}, nil
}

// ---------------------------------------------------------------------------
// Create bill
// ---------------------------------------------------------------------------

// createBillParams groups the inputs for inserting a new bill row.
type createBillParams struct {
	ID          uuid.UUID
	AccountID   string
	Currency    Currency
	PeriodStart time.Time
	PeriodEnd   time.Time
	WorkflowID  string
}

// createBill inserts a new OPEN bill and returns it with DB-generated timestamps.
func createBill(ctx context.Context, p createBillParams) (*Bill, error) {
	var createdAt, updatedAt time.Time
	err := billingDB.QueryRow(ctx, `
		INSERT INTO bills (id, account_id, currency, status, created_at, updated_at, period_start, period_end, total_minor, workflow_id)
		VALUES ($1, $2, $3, 'OPEN', NOW(), NOW(), $4, $5, 0, $6)
		RETURNING created_at, updated_at
	`, p.ID, p.AccountID, string(p.Currency), p.PeriodStart, p.PeriodEnd, p.WorkflowID).Scan(&createdAt, &updatedAt)
	if err != nil {
		return nil, errs.Wrap(err, "insert bill")
	}
	return &Bill{
		ID:          p.ID.String(),
		AccountID:   p.AccountID,
		Currency:    p.Currency,
		Status:      BillStatusOpen,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		PeriodStart: p.PeriodStart,
		PeriodEnd:   p.PeriodEnd,
		TotalMinor:  0,
	}, nil
}

// ---------------------------------------------------------------------------
// Bill workflow helpers
// ---------------------------------------------------------------------------

// setBillWorkflowRunID persists the Temporal run ID for a bill.
func setBillWorkflowRunID(ctx context.Context, billID uuid.UUID, runID string) error {
	_, err := billingDB.Exec(ctx, `
		UPDATE bills SET workflow_run_id = $2, updated_at = NOW()
		WHERE id = $1
	`, billID, runID)
	if err != nil {
		return errs.Wrap(err, "update workflow run id")
	}
	return nil
}

// cancelBill transitions an OPEN bill to CANCELLED. No-ops are logged as warnings.
func cancelBill(ctx context.Context, billID uuid.UUID) error {
	result, err := billingDB.Exec(ctx, `
		UPDATE bills SET status = 'CANCELLED', closed_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'OPEN'
	`, billID)
	if err != nil {
		return errs.Wrap(err, "cancel bill")
	}
	if result.RowsAffected() == 0 {
		log.Printf("WARNING: cancel bill %s was no-op (bill may not exist or already transitioned)", billID)
	}
	return nil
}

// NOTE: addLineItem and closeBill writes are now delegated to the Temporal
// worker via activities (PersistLineItem and MarkClosedInDB). The API layer
// communicates with the workflow through Updates (add-line-item) and Signals
// (close-bill). Only pure DB reads remain in the API service.

// ---------------------------------------------------------------------------
// Get bill
// ---------------------------------------------------------------------------

// getBill loads a single bill by ID, returning NotFound if it does not exist.
func getBill(ctx context.Context, billID uuid.UUID) (*Bill, error) {
	row := billingDB.QueryRow(ctx, `
		SELECT `+billColumns+` FROM bills WHERE id = $1
	`, billID)
	b, err := scanBill(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errs.B().Code(errs.NotFound).Msg("bill not found").Err()
		}
		return nil, errs.Wrap(err, "load bill")
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// List bills (paginated)
// ---------------------------------------------------------------------------

// billWorkflowRef pairs a bill ID with its Temporal workflow ID.
type billWorkflowRef struct {
	BillID     uuid.UUID
	WorkflowID string
}

// listBillsParams groups filter and pagination inputs for listing bills.
type listBillsParams struct {
	Status    *BillStatus
	AccountID string
	Limit     int
	Offset    int
}

// listBills returns a filtered, paginated slice of bills and the total count.
func listBills(ctx context.Context, p listBillsParams) ([]Bill, int, error) {
	// Build WHERE clause dynamically with parameterised values.
	// NOTE: hand-rolled arg-index tracking; consider squirrel or sqlc if filter count grows.
	var where []string
	var args []interface{}
	argIdx := 1

	if p.Status != nil {
		where = append(where, fmt.Sprintf("status = $%d", argIdx))
		args = append(args, string(*p.Status))
		argIdx++
	}
	if p.AccountID != "" {
		where = append(where, fmt.Sprintf("account_id = $%d", argIdx))
		args = append(args, p.AccountID)
		argIdx++
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	// Total count for pagination metadata.
	var total int
	countQuery := "SELECT COUNT(*) FROM bills " + whereClause
	if err := billingDB.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, errs.Wrap(err, "count bills")
	}

	// Fetch the requested page.
	dataQuery := fmt.Sprintf(
		"SELECT %s FROM bills %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d",
		billColumns, whereClause, argIdx, argIdx+1,
	)
	pageArgs := append(args, p.Limit, p.Offset) //nolint:gocritic // intentional new slice
	rows, err := billingDB.Query(ctx, dataQuery, pageArgs...)
	if err != nil {
		return nil, 0, errs.Wrap(err, "query bills")
	}
	defer rows.Close()

	bills := make([]Bill, 0)
	for rows.Next() {
		b, err := scanBill(rows)
		if err != nil {
			return nil, 0, errs.Wrap(err, "scan bill")
		}
		bills = append(bills, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, errs.Wrap(err, "rows error")
	}
	return bills, total, nil
}

// listBillsMissingWorkflowRunID returns bills that still need their run ID back-filled.
func listBillsMissingWorkflowRunID(ctx context.Context, limit int) ([]billWorkflowRef, error) {
	rows, err := billingDB.Query(ctx, `
		SELECT id, workflow_id
		FROM bills
		WHERE workflow_run_id IS NULL
		ORDER BY created_at ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, errs.Wrap(err, "query bills missing workflow run id")
	}
	defer rows.Close()

	items := make([]billWorkflowRef, 0)
	for rows.Next() {
		var item billWorkflowRef
		if err := rows.Scan(&item.BillID, &item.WorkflowID); err != nil {
			return nil, errs.Wrap(err, "scan bill missing workflow run id")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, "rows error")
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// Composite reads
// ---------------------------------------------------------------------------

// loadBillWithItems fetches a bill and its line items from the database,
// returning a combined response. Used by GetBill and UpdateBill handlers.
func loadBillWithItems(ctx context.Context, id uuid.UUID) (*BillWithItems, error) {
	b, err := getBill(ctx, id)
	if err != nil {
		return nil, err
	}
	items, err := listLineItems(ctx, id)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []LineItem{}
	}
	return &BillWithItems{Bill: *b, Items: items}, nil
}

// ---------------------------------------------------------------------------
// Line items
// ---------------------------------------------------------------------------

// listLineItems returns all line items for a bill ordered by creation time.
func listLineItems(ctx context.Context, billID uuid.UUID) ([]LineItem, error) {
	rows, err := billingDB.Query(ctx, `
		SELECT id, bill_id, idempotency_key, description, amount_minor, created_at
		FROM line_items WHERE bill_id = $1 ORDER BY created_at ASC
	`, billID)
	if err != nil {
		return nil, errs.Wrap(err, "query line items")
	}
	defer rows.Close()

	items := make([]LineItem, 0)
	for rows.Next() {
		var (
			id          uuid.UUID
			bid         uuid.UUID
			idemKey     string
			description string
			amountMinor int64
			createdAt   time.Time
		)
		if err := rows.Scan(&id, &bid, &idemKey, &description, &amountMinor, &createdAt); err != nil {
			return nil, errs.Wrap(err, "scan line item")
		}
		items = append(items, LineItem{
			ID:             id.String(),
			BillID:         bid.String(),
			IdempotencyKey: idemKey,
			Description:    description,
			AmountMinor:    MinorUnit(amountMinor),
			CreatedAt:      createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, "rows error")
	}
	return items, nil
}
