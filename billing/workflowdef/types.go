package workflowdef

import (
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// Sentinel errors
// ---------------------------------------------------------------------------

// Sentinel errors used by activities to signal non-retryable business conditions.
// These are matched by the API layer (via Temporal ApplicationError type codes)
// to return the appropriate gRPC/HTTP status codes to callers.
var (
	ErrBillNotFound    = errors.New("bill not found")
	ErrBillNotOpen     = errors.New("bill is not open")
	ErrPayloadMismatch = errors.New("idempotency_key already used with different payload")
)

// ---------------------------------------------------------------------------
// Workflow & activity payloads (must be JSON-serialisable)
// ---------------------------------------------------------------------------

// BillingWorkflowInput is the initial input when starting a billing workflow.
type BillingWorkflowInput struct {
	BillID    string    `json:"bill_id"`
	PeriodEnd time.Time `json:"period_end"`
}

// AddLineItemUpdateInput is the payload sent to the running workflow via Update.
type AddLineItemUpdateInput struct {
	Description    string `json:"description"`
	AmountMinor    int64  `json:"amount_minor"`
	IdempotencyKey string `json:"idempotency_key"`
	PayloadHash    string `json:"payload_hash"`
}

// PersistLineItemInput is the payload sent to the PersistLineItem activity.
type PersistLineItemInput struct {
	BillID         string `json:"bill_id"`
	Description    string `json:"description"`
	AmountMinor    int64  `json:"amount_minor"`
	IdempotencyKey string `json:"idempotency_key"`
	PayloadHash    string `json:"payload_hash"`
}

// PersistLineItemResult is returned by the PersistLineItem activity and
// propagated back through the Update to the API caller.
type PersistLineItemResult struct {
	Inserted bool       `json:"inserted"`
	Item     WfLineItem `json:"item"`
	Bill     WfBill     `json:"bill"`
}

// ---------------------------------------------------------------------------
// Domain representations (Temporal-serialisable, intentionally untyped
// for Currency/Status to avoid coupling workflowdef → billing)
// ---------------------------------------------------------------------------

// WfLineItem is the Temporal-serialisable representation of a line item.
type WfLineItem struct {
	ID             string    `json:"id"`
	BillID         string    `json:"bill_id"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	Description    string    `json:"description"`
	AmountMinor    int64     `json:"amount_minor"`
	CreatedAt      time.Time `json:"created_at"`
}

// WfBill is the Temporal-serialisable representation of a bill.
type WfBill struct {
	ID          string     `json:"id"`
	AccountID   string     `json:"account_id"`
	Currency    string     `json:"currency"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PeriodStart time.Time  `json:"period_start"`
	PeriodEnd   time.Time  `json:"period_end"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	TotalMinor  int64      `json:"total_minor"`
}
