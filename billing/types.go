package billing

import (
	"fmt"
	"time"
)

// MinorUnit represents a monetary amount in the smallest denomination (for example, cents for USD or tetri for GEL).
type MinorUnit int64

// Currency represents the system of money used in a country. For this assignment, we are only considering USD and GEL.
type Currency string

const (
	CurrencyUSD Currency = "USD"
	CurrencyGEL Currency = "GEL"
)

func (c Currency) Validate() error {
	switch c {
	case CurrencyUSD, CurrencyGEL:
		return nil
	default:
		return fmt.Errorf("unsupported currency %q", c)
	}
}

type BillStatus string

const (
	BillStatusOpen      BillStatus = "OPEN"
	BillStatusClosed    BillStatus = "CLOSED"
	BillStatusCancelled BillStatus = "CANCELLED"
)

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

type LineItem struct {
	ID             string    `json:"id"`
	BillID         string    `json:"bill_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Description    string    `json:"description"`
	AmountMinor    MinorUnit `json:"amount_minor"`
	CreatedAt      time.Time `json:"created_at"`
}

type BillWithItems struct {
	Bill  Bill       `json:"bill"`
	Items []LineItem `json:"items"`
}
