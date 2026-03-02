package main

import (
	"context"
	"pave-assignment/billing/workflowdef"
	"testing"
)

func TestActivities_CallInjectedDomainFuncs(t *testing.T) {
	t.Parallel()
	var (
		closedCalled  bool
		emailedCalled bool
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
	)
	if err := activities.MarkClosedInDB(context.Background(), "bill-1"); err != nil {
		t.Fatalf("expected close success, got %v", err)
	}
	if err := activities.SendInvoiceEmail(context.Background(), "bill-1"); err != nil {
		t.Fatalf("expected email success, got %v", err)
	}
	if !closedCalled || !emailedCalled {
		t.Fatalf("expected both injected callbacks to be called")
	}
}

func TestActivities_NilCallbacksReturnError(t *testing.T) {
	t.Parallel()
	activities := workflowdef.NewActivities(nil, nil)
	if err := activities.MarkClosedInDB(context.Background(), "bill-1"); err == nil {
		t.Fatalf("expected error from nil closeBill callback")
	}
	if err := activities.SendInvoiceEmail(context.Background(), "bill-1"); err == nil {
		t.Fatalf("expected error from nil sendInvoice callback")
	}
}
