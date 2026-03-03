package billing

import (
	"fmt"
	"time"
)

// MinorUnit represents a monetary amount in its smallest denomination (e.g. cents for USD, tetri for GEL).
type MinorUnit int64

// Currency identifies a supported monetary currency (USD, GEL).
type Currency string

const (
	CurrencyUSD Currency = "USD"
	CurrencyGEL Currency = "GEL"
)

// Validate returns an error if c is not a supported currency.
func (c Currency) Validate() error {
	switch c {
	case CurrencyUSD, CurrencyGEL:
		return nil
	default:
		return fmt.Errorf("unsupported currency %q", c)
	}
}

// BillStatus represents the lifecycle state of a bill.
type BillStatus string

const (
	BillStatusOpen      BillStatus = "OPEN"
	BillStatusClosed    BillStatus = "CLOSED"
	BillStatusCancelled BillStatus = "CANCELLED"
)

// Bill is the API-facing representation of a billing period.
type Bill struct {
	ID          string     `json:"id"`
	AccountID   string     `json:"account_id"`
	Currency    Currency   `json:"currency"`
	Status      BillStatus `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PeriodStart time.Time  `json:"period_start"`
	PeriodEnd   time.Time  `json:"period_end"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	TotalMinor  MinorUnit  `json:"total_minor"`
}

// LineItem is a single charge within a bill.
type LineItem struct {
	ID             string    `json:"id"`
	BillID         string    `json:"bill_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Description    string    `json:"description"`
	AmountMinor    MinorUnit `json:"amount_minor"`
	CreatedAt      time.Time `json:"created_at"`
}

// BillWithItems bundles a bill with its line items for detail responses.
type BillWithItems struct {
	Bill  Bill       `json:"bill"`
	Items []LineItem `json:"items"`
}
