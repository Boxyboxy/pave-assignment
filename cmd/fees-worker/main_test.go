package main

import (
	"context"
	"pave-assignment/billing/workflowdef"
	"testing"
	"time"
)

func TestActivities_CallInjectedDomainFuncs(t *testing.T) {
	t.Parallel()
	var (
		closedCalled  bool
		emailedCalled bool
		persistCalled bool
	)
	activities := workflowdef.NewActivities(
		func(context.Context, string) error {
			closedCalled = true
			return nil
		},
		func(context.Context, string) error {
			emailedCalled = true
			return nil
		},
		func(_ context.Context, _ workflowdef.PersistLineItemInput) (*workflowdef.PersistLineItemResult, error) {
			persistCalled = true
			return &workflowdef.PersistLineItemResult{
				Inserted: true,
				Item: workflowdef.WfLineItem{
					ID:          "item-1",
					BillID:      "bill-1",
					Description: "test",
					AmountMinor: 100,
					CreatedAt:   time.Now(),
				},
				Bill: workflowdef.WfBill{
					ID:         "bill-1",
					TotalMinor: 100,
				},
			}, nil
		},
	)
	if err := activities.MarkClosedInDB(context.Background(), "bill-1"); err != nil {
		t.Fatalf("expected close success, got %v", err)
	}
	if err := activities.SendInvoiceEmail(context.Background(), "bill-1"); err != nil {
		t.Fatalf("expected email success, got %v", err)
	}
	result, err := activities.PersistLineItem(context.Background(), workflowdef.PersistLineItemInput{
		BillID:         "bill-1",
		Description:    "test",
		AmountMinor:    100,
		IdempotencyKey: "key-1",
		PayloadHash:    "hash-1",
	})
	if err != nil {
		t.Fatalf("expected persist success, got %v", err)
	}
	if !result.Inserted {
		t.Fatalf("expected inserted=true")
	}
	if !closedCalled || !emailedCalled || !persistCalled {
		t.Fatalf("expected all injected callbacks to be called")
	}
}

func TestActivities_NilCallbacksReturnError(t *testing.T) {
	t.Parallel()
	activities := workflowdef.NewActivities(nil, nil, nil)
	if err := activities.MarkClosedInDB(context.Background(), "bill-1"); err == nil {
		t.Fatalf("expected error from nil closeBill callback")
	}
	if err := activities.SendInvoiceEmail(context.Background(), "bill-1"); err == nil {
		t.Fatalf("expected error from nil sendInvoice callback")
	}
	if _, err := activities.PersistLineItem(context.Background(), workflowdef.PersistLineItemInput{}); err == nil {
		t.Fatalf("expected error from nil persistLineItem callback")
	}
}
