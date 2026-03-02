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

var billingDB = sqldb.NewDatabase("billing", sqldb.DatabaseConfig{
	Migrations: "./migrations",
})

// ---------------------------------------------------------------------------
// Bill columns constant (used by all bill queries)
// ---------------------------------------------------------------------------

const billColumns = "id, account_id, currency, status, created_at, updated_at, period_start, period_end, closed_at, total_minor"

// scanBill scans a row with the standard bill column set into a Bill struct.
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
		TotalMinor:  totalMinor,
	}, nil
}

// ---------------------------------------------------------------------------
// Create bill
// ---------------------------------------------------------------------------

type createBillParams struct {
	ID          uuid.UUID
	AccountID   string
	Currency    Currency
	PeriodStart time.Time
	PeriodEnd   time.Time
	WorkflowID  string
}

func createBill(ctx context.Context, p createBillParams) (*Bill, error) {
	now := time.Now().UTC()
	b := &Bill{
		ID:          p.ID.String(),
		AccountID:   p.AccountID,
		Currency:    p.Currency,
		Status:      BillStatusOpen,
		CreatedAt:   now,
		UpdatedAt:   now,
		PeriodStart: p.PeriodStart,
		PeriodEnd:   p.PeriodEnd,
		TotalMinor:  0,
	}
	_, err := billingDB.Exec(ctx, `
		INSERT INTO bills (id, account_id, currency, status, created_at, updated_at, period_start, period_end, total_minor, workflow_id)
		VALUES ($1, $2, $3, 'OPEN', $4, $4, $5, $6, 0, $7)
	`, p.ID, p.AccountID, string(p.Currency), b.CreatedAt, b.PeriodStart, b.PeriodEnd, p.WorkflowID)
	if err != nil {
		return nil, errs.Wrap(err, "insert bill")
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// Bill workflow helpers
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Add line item
// ---------------------------------------------------------------------------

type addLineItemParams struct {
	BillID         uuid.UUID
	Description    string
	AmountMinor    int64
	IdempotencyKey string
	PayloadHash    string
}

type addLineItemResult struct {
	Item LineItem
	Bill Bill
}

func addLineItem(ctx context.Context, p addLineItemParams) (*addLineItemResult, error) {
	if p.IdempotencyKey == "" {
		return nil, errs.B().Code(errs.InvalidArgument).Msg("idempotency_key is required").Err()
	}
	if p.PayloadHash == "" {
		return nil, errs.B().Code(errs.InvalidArgument).Msg("payload hash is required").Err()
	}

	tx, err := billingDB.Begin(ctx)
	if err != nil {
		return nil, errs.Wrap(err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	var (
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
	err = tx.QueryRow(ctx, `
		SELECT account_id, currency, status, created_at, updated_at, period_start, period_end, closed_at, total_minor
		FROM bills WHERE id = $1
		FOR UPDATE
	`, p.BillID).Scan(&accountID, &currency, &status, &createdAt, &updatedAt, &periodStart, &periodEnd, &closedAt, &totalMinor)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errs.B().Code(errs.NotFound).Msg("bill not found").Err()
		}
		return nil, errs.Wrap(err, "load bill")
	}
	if status != string(BillStatusOpen) {
		return nil, errs.B().Code(errs.FailedPrecondition).Msgf("bill is %s", status).Err()
	}

	itemID := uuid.New()
	itemCreatedAt := time.Now().UTC()
	var inserted bool

	var returnedID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO line_items (id, bill_id, idempotency_key, payload_hash, description, amount_minor, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (bill_id, idempotency_key) DO NOTHING
		RETURNING id
	`, itemID, p.BillID, p.IdempotencyKey, p.PayloadHash, p.Description, p.AmountMinor, itemCreatedAt).Scan(&returnedID)
	if err == nil {
		inserted = true
		itemID = returnedID
	} else if errors.Is(err, sql.ErrNoRows) {
		var (
			existingDesc    string
			existingAmount  int64
			existingCreated time.Time
			storedHash      string
		)
		err = tx.QueryRow(ctx, `
			SELECT id, description, amount_minor, created_at, payload_hash
			FROM line_items WHERE bill_id = $1 AND idempotency_key = $2
		`, p.BillID, p.IdempotencyKey).Scan(&returnedID, &existingDesc, &existingAmount, &existingCreated, &storedHash)
		if err != nil {
			return nil, errs.Wrap(err, "load existing line item")
		}
		// Backward-compatible path for pre-hash rows migrated with empty payload_hash.
		if storedHash == "" {
			if _, err := tx.Exec(ctx, `
				UPDATE line_items SET payload_hash = $3
				WHERE bill_id = $1 AND idempotency_key = $2
			`, p.BillID, p.IdempotencyKey, p.PayloadHash); err != nil {
				return nil, errs.Wrap(err, "backfill line item payload hash")
			}
			storedHash = p.PayloadHash
		}
		if storedHash != p.PayloadHash {
			return nil, errs.B().Code(errs.AlreadyExists).Msg("idempotency_key already used with different payload").Err()
		}
		itemID = returnedID
		// Use the stored values for the response (not the incoming params).
		p.Description = existingDesc
		p.AmountMinor = existingAmount
		itemCreatedAt = existingCreated
		inserted = false
	} else {
		return nil, errs.Wrap(err, "insert line item")
	}

	if inserted {
		err = tx.QueryRow(ctx, `
			UPDATE bills SET total_minor = total_minor + $2, updated_at = NOW()
			WHERE id = $1
			RETURNING total_minor, updated_at
		`, p.BillID, p.AmountMinor).Scan(&totalMinor, &updatedAt)
		if err != nil {
			return nil, errs.Wrap(err, "update bill total")
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, errs.Wrap(err, "commit tx")
	}

	var closedAtPtr *time.Time
	if closedAt.Valid {
		closedAtPtr = &closedAt.Time
	}
	return &addLineItemResult{
		Item: LineItem{
			ID:             itemID.String(),
			BillID:         p.BillID.String(),
			IdempotencyKey: optionalString(p.IdempotencyKey),
			Description:    p.Description,
			AmountMinor:    p.AmountMinor,
			CreatedAt:      itemCreatedAt,
		},
		Bill: Bill{
			ID:          p.BillID.String(),
			AccountID:   accountID,
			Currency:    Currency(currency),
			Status:      BillStatus(status),
			CreatedAt:   createdAt,
			UpdatedAt:   updatedAt,
			PeriodStart: periodStart,
			PeriodEnd:   periodEnd,
			ClosedAt:    closedAtPtr,
			TotalMinor:  totalMinor,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// Close bill (delegates mutation to shared workflowdef.CloseBillInDB)
// ---------------------------------------------------------------------------

func closeBill(ctx context.Context, billID uuid.UUID) (*Bill, error) {
	tx, err := billingDB.Begin(ctx)
	if err != nil {
		return nil, errs.Wrap(err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the bill row to serialize with concurrent addLineItem transactions.
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM bills WHERE id = $1 FOR UPDATE`, billID).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errs.B().Code(errs.NotFound).Msg("bill not found").Err()
		}
		return nil, errs.Wrap(err, "lock bill for close")
	}

	if status == string(BillStatusOpen) {
		_, err = tx.Exec(ctx, `
			UPDATE bills SET status = 'CLOSED', closed_at = NOW(), updated_at = NOW()
			WHERE id = $1
		`, billID)
		if err != nil {
			return nil, errs.Wrap(err, "update bill status to closed")
		}
	}
	// If already CLOSED or CANCELLED, no-op (idempotent).

	row := tx.QueryRow(ctx, `SELECT `+billColumns+` FROM bills WHERE id = $1`, billID)
	b, err := scanBill(row)
	if err != nil {
		return nil, errs.Wrap(err, "read bill after close")
	}

	if err := tx.Commit(); err != nil {
		return nil, errs.Wrap(err, "commit close tx")
	}

	return b, nil
}

// ---------------------------------------------------------------------------
// Get bill
// ---------------------------------------------------------------------------

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

func getBillWorkflowID(ctx context.Context, billID uuid.UUID) (string, error) {
	var workflowID string
	err := billingDB.QueryRow(ctx, `SELECT workflow_id FROM bills WHERE id = $1`, billID).Scan(&workflowID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errs.B().Code(errs.NotFound).Msg("bill not found").Err()
		}
		return "", errs.Wrap(err, "load bill workflow id")
	}
	return workflowID, nil
}

// ---------------------------------------------------------------------------
// List bills (paginated)
// ---------------------------------------------------------------------------

type billWorkflowRef struct {
	BillID     uuid.UUID
	WorkflowID string
}

type listBillsParams struct {
	Status    *BillStatus
	AccountID string
	Limit     int
	Offset    int
}

func listBills(ctx context.Context, p listBillsParams) ([]Bill, int, error) {
	// Build WHERE clause dynamically with parameterised values.
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
// Line items
// ---------------------------------------------------------------------------

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
			idemKey     sql.NullString
			description string
			amountMinor int64
			createdAt   time.Time
		)
		if err := rows.Scan(&id, &bid, &idemKey, &description, &amountMinor, &createdAt); err != nil {
			return nil, errs.Wrap(err, "scan line item")
		}
		var keyPtr *string
		if idemKey.Valid {
			keyPtr = &idemKey.String
		}
		items = append(items, LineItem{
			ID:             id.String(),
			BillID:         bid.String(),
			IdempotencyKey: keyPtr,
			Description:    description,
			AmountMinor:    amountMinor,
			CreatedAt:      createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, "rows error")
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
