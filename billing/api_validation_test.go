package billing

import (
	"strings"
	"testing"
	"time"
)

func TestCurrencyValidate(t *testing.T) {
	t.Parallel()
	if err := CurrencyUSD.Validate(); err != nil {
		t.Fatalf("expected USD valid: %v", err)
	}
	if err := CurrencyGEL.Validate(); err != nil {
		t.Fatalf("expected GEL valid: %v", err)
	}
	if err := Currency("EUR").Validate(); err == nil {
		t.Fatalf("expected unsupported currency error")
	}
	if err := Currency("").Validate(); err == nil {
		t.Fatalf("expected empty currency error")
	}
}

func TestCreateBillValidation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	if _, err := validateCreateBillRequest(nil, now); err == nil {
		t.Fatalf("expected nil request error")
	}
	// Missing account_id
	if _, err := validateCreateBillRequest(&CreateBillRequest{
		Currency:  CurrencyUSD,
		PeriodEnd: now.Add(time.Hour),
	}, now); err == nil {
		t.Fatalf("expected missing account_id error")
	}
	// account_id too long
	if _, err := validateCreateBillRequest(&CreateBillRequest{
		AccountID: strings.Repeat("a", 201),
		Currency:  CurrencyUSD,
		PeriodEnd: now.Add(time.Hour),
	}, now); err == nil {
		t.Fatalf("expected account_id too long error")
	}
	// Unsupported currency
	if _, err := validateCreateBillRequest(&CreateBillRequest{
		AccountID: "acct-1",
		Currency:  Currency("EUR"),
		PeriodEnd: now.Add(time.Hour),
	}, now); err == nil {
		t.Fatalf("expected unsupported currency error")
	}
	// Period end in the past
	if _, err := validateCreateBillRequest(&CreateBillRequest{
		AccountID: "acct-1",
		Currency:  CurrencyUSD,
		PeriodEnd: now.Add(-time.Minute),
	}, now); err == nil {
		t.Fatalf("expected past period_end error")
	}
	// Period end too far in the future
	if _, err := validateCreateBillRequest(&CreateBillRequest{
		AccountID: "acct-1",
		Currency:  CurrencyUSD,
		PeriodEnd: now.Add(366 * 24 * time.Hour),
	}, now); err == nil {
		t.Fatalf("expected too-far-in-future period_end error")
	}
	// Zero period_end
	if _, err := validateCreateBillRequest(&CreateBillRequest{
		AccountID: "acct-1",
		Currency:  CurrencyUSD,
		PeriodEnd: time.Time{},
	}, now); err == nil {
		t.Fatalf("expected zero period_end error")
	}
	// Valid request
	if _, err := validateCreateBillRequest(&CreateBillRequest{
		AccountID: "acct-1",
		Currency:  CurrencyUSD,
		PeriodEnd: now.Add(5 * time.Minute),
	}, now); err != nil {
		t.Fatalf("expected valid create bill request, got %v", err)
	}
	// Account ID should be trimmed
	req := &CreateBillRequest{
		AccountID: "  acct-1  ",
		Currency:  CurrencyUSD,
		PeriodEnd: now.Add(5 * time.Minute),
	}
	if _, err := validateCreateBillRequest(req, now); err != nil {
		t.Fatalf("expected valid create bill request, got %v", err)
	}
	if req.AccountID != "acct-1" {
		t.Fatalf("expected account_id to be trimmed, got %q", req.AccountID)
	}
}

func TestAddLineItemValidation(t *testing.T) {
	t.Parallel()
	if _, _, err := validateAddLineItemRequest("not-a-uuid", nil); err == nil {
		t.Fatalf("expected nil request error")
	}
	if _, _, err := validateAddLineItemRequest("not-a-uuid", &AddLineItemRequest{
		Description: "fee",
		AmountMinor: 100,
	}); err == nil {
		t.Fatalf("expected invalid bill id error")
	}
	validID := "8cbf374a-7b61-4d03-8ff8-e46265d66abc"
	if _, _, err := validateAddLineItemRequest(validID, &AddLineItemRequest{
		Description: " ",
		AmountMinor: 100,
	}); err == nil {
		t.Fatalf("expected empty description error")
	}
	if _, _, err := validateAddLineItemRequest(validID, &AddLineItemRequest{
		Description: "fee",
		AmountMinor: 0,
	}); err == nil {
		t.Fatalf("expected zero amount error")
	}
	// Negative amount
	if _, _, err := validateAddLineItemRequest(validID, &AddLineItemRequest{
		Description:    "fee",
		AmountMinor:    -100,
		IdempotencyKey: "key-1",
	}); err == nil {
		t.Fatalf("expected negative amount error")
	}
	if _, _, err := validateAddLineItemRequest(validID, &AddLineItemRequest{
		Description: "fee",
		AmountMinor: 100,
	}); err == nil {
		t.Fatalf("expected missing idempotency key error")
	}
	// Whitespace-only idempotency key
	if _, _, err := validateAddLineItemRequest(validID, &AddLineItemRequest{
		Description:    "fee",
		AmountMinor:    100,
		IdempotencyKey: "   ",
	}); err == nil {
		t.Fatalf("expected whitespace-only idempotency key error")
	}
	if _, _, err := validateAddLineItemRequest(validID, &AddLineItemRequest{
		Description:    "fee",
		AmountMinor:    100,
		IdempotencyKey: strings.Repeat("x", 201),
	}); err == nil {
		t.Fatalf("expected idempotency key length error")
	}
	req := &AddLineItemRequest{
		Description:    "  fee  ",
		AmountMinor:    100,
		IdempotencyKey: "  key-1  ",
	}
	if _, payloadHash, err := validateAddLineItemRequest(validID, req); err != nil {
		t.Fatalf("expected valid request, got %v", err)
	} else if payloadHash == "" {
		t.Fatalf("expected non-empty payload hash")
	}
	if req.Description != "fee" || req.IdempotencyKey != "key-1" {
		t.Fatalf("expected request fields to be trimmed")
	}
}

func TestParseBillStatusFilter(t *testing.T) {
	t.Parallel()
	if st, err := parseBillStatusFilter(nil); err != nil || st != nil {
		t.Fatalf("expected nil,nil for empty status")
	}

	open := "open"
	st, err := parseBillStatusFilter(&open)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st == nil || *st != BillStatusOpen {
		t.Fatalf("expected OPEN status")
	}

	closed := " CLOSED "
	st, err = parseBillStatusFilter(&closed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st == nil || *st != BillStatusClosed {
		t.Fatalf("expected CLOSED status")
	}

	cancelled := "cancelled"
	st, err = parseBillStatusFilter(&cancelled)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st == nil || *st != BillStatusCancelled {
		t.Fatalf("expected CANCELLED status")
	}

	invalid := "active"
	if _, err := parseBillStatusFilter(&invalid); err == nil {
		t.Fatalf("expected invalid status error")
	}
}

func TestParseBillID(t *testing.T) {
	t.Parallel()
	if _, err := parseBillID("bad-id"); err == nil {
		t.Fatalf("expected parse error for invalid bill id")
	}
	if _, err := parseBillID(""); err == nil {
		t.Fatalf("expected parse error for empty bill id")
	}
	if _, err := parseBillID("8cbf374a-7b61-4d03-8ff8-e46265d66abc"); err != nil {
		t.Fatalf("expected valid bill id, got %v", err)
	}
}

func TestComputePayloadHash(t *testing.T) {
	t.Parallel()

	// Deterministic: same inputs produce same hash.
	h1 := computeLineItemPayloadHash("fee", 100)
	h2 := computeLineItemPayloadHash("fee", 100)
	if h1 != h2 {
		t.Fatalf("expected deterministic hash, got %q and %q", h1, h2)
	}
	if h1 == "" {
		t.Fatalf("expected non-empty hash")
	}

	// Different amount produces different hash.
	h3 := computeLineItemPayloadHash("fee", 200)
	if h1 == h3 {
		t.Fatalf("expected different hash for different amount")
	}

	// Different description produces different hash.
	h4 := computeLineItemPayloadHash("other fee", 100)
	if h1 == h4 {
		t.Fatalf("expected different hash for different description")
	}

	// Edge case: empty description still produces a hash.
	h5 := computeLineItemPayloadHash("", 100)
	if h5 == "" {
		t.Fatalf("expected non-empty hash for empty description")
	}
	if h5 == h1 {
		t.Fatalf("expected different hash for empty vs non-empty description")
	}
}
