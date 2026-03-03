package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Request parsing
// ---------------------------------------------------------------------------

// parseCreateBillRequest validates required fields and returns the normalised period_end.
func parseCreateBillRequest(req *CreateBillRequest, now time.Time) (time.Time, error) {
	if req == nil {
		return time.Time{}, fmt.Errorf("missing request body")
	}
	req.AccountID = strings.TrimSpace(req.AccountID)
	if req.AccountID == "" {
		return time.Time{}, fmt.Errorf("account_id is required")
	}
	if len(req.AccountID) > 200 {
		return time.Time{}, fmt.Errorf("account_id too long")
	}
	if err := req.Currency.Validate(); err != nil {
		return time.Time{}, err
	}
	periodEnd := req.PeriodEnd.UTC()
	if periodEnd.IsZero() {
		return time.Time{}, fmt.Errorf("period_end is required")
	}
	if !periodEnd.After(now) {
		return time.Time{}, fmt.Errorf("period_end must be in the future")
	}
	if periodEnd.Sub(now) > 365*24*time.Hour {
		return time.Time{}, fmt.Errorf("period_end too far in the future")
	}
	return periodEnd, nil
}

// parseAddLineItemRequest validates required fields and returns the parsed bill UUID
// and a SHA-256 payload hash used for idempotency verification.
func parseAddLineItemRequest(billID string, req *AddLineItemRequest) (uuid.UUID, string, error) {
	if req == nil {
		return uuid.Nil, "", fmt.Errorf("missing request body")
	}
	id, err := parseBillID(billID)
	if err != nil {
		return uuid.Nil, "", err
	}
	req.Description = strings.TrimSpace(req.Description)
	if req.Description == "" {
		return uuid.Nil, "", fmt.Errorf("description is required")
	}
	if req.AmountMinor <= 0 {
		return uuid.Nil, "", fmt.Errorf("amount_minor must be > 0")
	}
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.IdempotencyKey == "" {
		return uuid.Nil, "", fmt.Errorf("idempotency_key is required")
	}
	if len(req.IdempotencyKey) > 200 {
		return uuid.Nil, "", fmt.Errorf("idempotency_key too long")
	}
	return id, computeLineItemPayloadHash(req.Description, int64(req.AmountMinor)), nil
}

// parseBillStatusFilter converts an optional status string to a typed BillStatus pointer.
func parseBillStatusFilter(status *string) (*BillStatus, error) {
	if status == nil {
		return nil, nil
	}
	s := strings.ToUpper(strings.TrimSpace(*status))
	tmp := BillStatus(s)
	switch tmp {
	case BillStatusOpen, BillStatusClosed, BillStatusCancelled:
		return &tmp, nil
	default:
		return nil, fmt.Errorf("invalid status; expected OPEN, CLOSED, or CANCELLED")
	}
}

// parseBillID parses and validates a bill ID path parameter.
func parseBillID(billID string) (uuid.UUID, error) {
	id, err := uuid.Parse(billID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid bill_id")
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Payload hashing
// ---------------------------------------------------------------------------
// computeLineItemPayloadHash produces a deterministic SHA-256 hash of the
// line-item payload, used to detect conflicting idempotent retries.
func computeLineItemPayloadHash(description string, amountMinor int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", description, amountMinor)))
	return hex.EncodeToString(sum[:])
}
