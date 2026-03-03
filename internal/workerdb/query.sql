-- Queries used by the Temporal worker for bill mutations.
-- Source of truth for sqlc code generation (see sqlc.yaml at project root).
-- Read-only queries used by the API service remain in billing/store.go.

-- name: LockBillForUpdate :one
SELECT account_id, currency, status, created_at, updated_at,
       period_start, period_end, closed_at, total_minor
FROM bills WHERE id = $1 FOR UPDATE;

-- name: InsertLineItemIdempotent :one
INSERT INTO line_items (id, bill_id, idempotency_key, payload_hash, description, amount_minor, created_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (bill_id, idempotency_key) DO NOTHING
RETURNING id, created_at;

-- name: GetLineItemByBillAndKey :one
SELECT id, description, amount_minor, payload_hash, created_at
FROM line_items WHERE bill_id = $1 AND idempotency_key = $2;

-- name: IncrementBillTotal :one
UPDATE bills
SET total_minor = total_minor + sqlc.arg(amount), updated_at = NOW()
WHERE id = sqlc.arg(bill_id)
RETURNING total_minor, updated_at;

-- name: CloseBillByID :execresult
UPDATE bills
SET status = 'CLOSED', closed_at = COALESCE(closed_at, NOW()), updated_at = NOW()
WHERE id = $1 AND status = 'OPEN';

-- name: GetBillStatus :one
SELECT status FROM bills WHERE id = $1;
