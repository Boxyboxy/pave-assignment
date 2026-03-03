package billing

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"pave-assignment/billing/workflowdef"

	"encore.dev/beta/errs"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Create bill
// ---------------------------------------------------------------------------

// CreateBillRequest is the payload for creating a new bill.
type CreateBillRequest struct {
	AccountID string    `json:"account_id"`
	Currency  Currency  `json:"currency"`
	PeriodEnd time.Time `json:"period_end"`
}

// CreateBill creates a new bill, inserts it into the DB (synchronous), and starts the
// corresponding Temporal billing workflow (fire and forget).
//
//encore:api public method=POST path=/bills
func (s *Service) CreateBill(ctx context.Context, req *CreateBillRequest) (*Bill, error) {
	now := time.Now().UTC()
	periodEnd, err := parseCreateBillRequest(req, now)
	if err != nil {
		return nil, errs.B().Code(errs.InvalidArgument).Msg(err.Error()).Err()
	}

	billID := uuid.New()
	workflowID := billingWorkflowID(billID)

	bill, err := createBill(ctx, createBillParams{
		ID:          billID,
		AccountID:   req.AccountID,
		Currency:    req.Currency,
		PeriodStart: now,
		PeriodEnd:   periodEnd,
		WorkflowID:  workflowID,
	})
	if err != nil {
		return nil, err
	}

	runID, err := s.startBillingWorkflow(ctx, workflowID, workflowdef.BillingWorkflowInput{
		BillID:    bill.ID,
		PeriodEnd: bill.PeriodEnd,
	})
	if err != nil {
		if cancelErr := cancelBill(ctx, billID); cancelErr != nil {
			log.Printf(
				"CRITICAL: failed to cancel bill after workflow start failure bill_id=%s cancel_err=%v original_err=%v",
				billID.String(), cancelErr, err,
			)
		}
		return nil, err
	}
	if err := setBillWorkflowRunID(ctx, billID, runID); err != nil {
		termErr := s.terminateBillingWorkflow(ctx, workflowID, runID, "workflow_run_id_persist_failed")
		if termErr != nil {
			log.Printf(
				"CRITICAL: failed to terminate workflow after run_id persist failure bill_id=%s workflow_id=%s run_id=%s persist_err=%v terminate_err=%v",
				billID.String(), workflowID, runID, err, termErr,
			)
		} else {
			log.Printf(
				"WARNING: workflow terminated after run_id persist failure bill_id=%s workflow_id=%s run_id=%s persist_err=%v",
				billID.String(), workflowID, runID, err,
			)
		}
		return nil, errs.B().
			Code(errs.Internal).
			Msg("failed to finalize bill creation after workflow start; please retry").
			Err()
	}
	return bill, nil
}

// ---------------------------------------------------------------------------
// Add line item (write delegated to Temporal workflow via Update)
// ---------------------------------------------------------------------------
// AddLineItemRequest is the payload for adding a line item to an open bill.
type AddLineItemRequest struct {
	Description    string    `json:"description"`
	AmountMinor    MinorUnit `json:"amount_minor"`
	IdempotencyKey string    `json:"idempotency_key"`
}

// AddLineItemResponse contains the updated bill and the persisted line item.
type AddLineItemResponse struct {
	Bill Bill     `json:"bill"`
	Item LineItem `json:"item"`
}

// AddLineItem delegates line-item creation to the Temporal workflow via Update,
// ensuring durable persistence and idempotency.
// Using Temporal Updates (synchronous counterpart to Signals).
//
//encore:api public method=POST path=/bills/:billID/items
func (s *Service) AddLineItem(ctx context.Context, billID string, req *AddLineItemRequest) (*AddLineItemResponse, error) {
	id, payloadHash, err := parseAddLineItemRequest(billID, req)
	if err != nil {
		return nil, errs.B().Code(errs.InvalidArgument).Msg(err.Error()).Err()
	}

	workflowID := billingWorkflowID(id)
	result, err := s.updateLineItemWorkflow(ctx, workflowID, workflowdef.AddLineItemUpdateInput{
		Description:    req.Description,
		AmountMinor:    int64(req.AmountMinor),
		IdempotencyKey: req.IdempotencyKey,
		PayloadHash:    payloadHash,
	})
	if err != nil {
		return nil, mapTemporalError(err)
	}

	return &AddLineItemResponse{
		Bill: wfBillToBill(result.Bill),
		Item: wfLineItemToLineItem(result.Item),
	}, nil
}

// ---------------------------------------------------------------------------
// Close bill (write delegated to Temporal workflow via Signal)
// ---------------------------------------------------------------------------
// UpdateBillRequest is the payload for updating a bill (currently only closing).
type UpdateBillRequest struct {
	Status string `json:"status"`
}

// UpdateBill closes a bill by signalling its Temporal workflow. If the
// workflow is already completed, a fallback workflow is started to close
// the bill via the MarkClosedInDB activity.
//
//encore:api public method=PATCH path=/bills/:billID
func (s *Service) UpdateBill(ctx context.Context, billID string, req *UpdateBillRequest) (*BillWithItems, error) {
	id, err := parseBillID(billID)
	if err != nil {
		return nil, errs.B().Code(errs.InvalidArgument).Msg(err.Error()).Err()
	}

	if req == nil {
		return nil, errs.B().Code(errs.InvalidArgument).Msg("missing request body").Err()
	}
	if strings.ToUpper(strings.TrimSpace(req.Status)) != string(BillStatusClosed) {
		return nil, errs.B().Code(errs.InvalidArgument).Msg("only status=CLOSED is supported").Err()
	}

	workflowID := billingWorkflowID(id)

	// Signal the running workflow to close. The workflow loop will break,
	// execute MarkClosedInDB + SendInvoice activities, then complete.
	signalErr := s.signalCloseBillingWorkflow(ctx, workflowID)
	if signalErr == nil {
		// Signal accepted — wait for workflow to finish (close + invoice),
		// but enforce a short upper bound on how long we block the caller.
		if err := s.waitForWorkflowWithTimeout(ctx, workflowID, closeWaitTimeout); err != nil {
			return nil, err
		}
	} else {
		log.Printf("WARNING: signal close failed bill_id=%s err=%v (checking DB state)", billID, signalErr)

		// Workflow may have already completed (timer auto-close). Check DB.
		b, err := getBill(ctx, id)
		if err != nil {
			return nil, err
		}
		if b.Status == BillStatusOpen {
			// Bill is still OPEN but workflow is unreachable — start a
			// fallback workflow with period_end = now so it closes immediately
			// via the MarkClosedInDB activity. Deterministic workflow ID
			// makes this idempotent across retries.
			fallbackID := fallbackCloseWorkflowID(id)
			if _, fbErr := s.startBillingWorkflow(ctx, fallbackID, workflowdef.BillingWorkflowInput{
				BillID:    id.String(),
				PeriodEnd: time.Now().UTC(),
			}); fbErr != nil {
				log.Printf("WARNING: fallback workflow start failed bill_id=%s err=%v", billID, fbErr)
				return nil, errs.B().Code(errs.Unavailable).
					Msg("unable to close bill: workflow service unavailable").Err()
			}
			if err := s.waitForWorkflowWithTimeout(ctx, fallbackID, closeWaitTimeout); err != nil {
				return nil, err
			}
		}
		// Already CLOSED or CANCELLED — fall through to DB read below.
	}

	return loadBillWithItems(ctx, id)
}

// ---------------------------------------------------------------------------
// Get bill
// ---------------------------------------------------------------------------

// GetBill returns a single bill with all its line items.
//
//encore:api public method=GET path=/bills/:billID
func (s *Service) GetBill(ctx context.Context, billID string) (*BillWithItems, error) {
	id, err := parseBillID(billID)
	if err != nil {
		return nil, errs.B().Code(errs.InvalidArgument).Msg(err.Error()).Err()
	}
	return loadBillWithItems(ctx, id)
}

// ---------------------------------------------------------------------------
// List bills (paginated)
// ---------------------------------------------------------------------------

// ListBillsRequest carries optional filters and pagination parameters.
type ListBillsRequest struct {
	Status    string `query:"status"`
	AccountID string `query:"account_id"`
	Limit     int    `query:"limit"`
	Offset    int    `query:"offset"`
}

// ListBillsResponse wraps the paginated bill list with total count metadata.
type ListBillsResponse struct {
	Bills  []Bill `json:"bills"`
	Total  int    `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

// ListBills returns a paginated list of bills with optional status/account filters.
//
//encore:api public method=GET path=/bills
func (s *Service) ListBills(ctx context.Context, req *ListBillsRequest) (*ListBillsResponse, error) {
	var status *string
	var accountID string
	limit := 50
	offset := 0

	if req != nil {
		statusStr := strings.TrimSpace(req.Status)
		if statusStr != "" {
			status = &statusStr
		}
		accountID = strings.TrimSpace(req.AccountID)
		if req.Limit > 0 {
			limit = req.Limit
		}
		if req.Offset > 0 {
			offset = req.Offset
		}
	}
	if limit > 200 {
		limit = 200
	}

	st, err := parseBillStatusFilter(status)
	if err != nil {
		return nil, errs.B().Code(errs.InvalidArgument).Msg(err.Error()).Err()
	}
	bills, total, err := listBills(ctx, listBillsParams{
		Status:    st,
		AccountID: accountID,
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		return nil, err
	}
	return &ListBillsResponse{
		Bills:  bills,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	}, nil
}

// ---------------------------------------------------------------------------
// Reconcile workflow run IDs (private / operational)
// ---------------------------------------------------------------------------

// ReconcileWorkflowRunIDsRequest controls the batch size for reconciliation.
type ReconcileWorkflowRunIDsRequest struct {
	Limit int `json:"limit"`
}

// ReconcileWorkflowRunIDsResponse reports reconciliation results.
type ReconcileWorkflowRunIDsResponse struct {
	Scanned    int      `json:"scanned"`
	Backfilled int      `json:"backfilled"`
	Failed     int      `json:"failed"`
	Errors     []string `json:"errors,omitempty"`
}

// ReconcileWorkflowRunIDs back-fills missing workflow_run_id values by
// describing each workflow in Temporal. Private/operational endpoint.
//
//encore:api private method=POST path=/internal/workflows/reconcile-run-ids
func (s *Service) ReconcileWorkflowRunIDs(ctx context.Context, req *ReconcileWorkflowRunIDsRequest) (*ReconcileWorkflowRunIDsResponse, error) {
	limit := 100
	if req != nil && req.Limit > 0 {
		limit = req.Limit
	}
	if limit > 500 {
		limit = 500
	}

	refs, err := listBillsMissingWorkflowRunID(ctx, limit)
	if err != nil {
		return nil, err
	}

	res := &ReconcileWorkflowRunIDsResponse{
		Scanned: len(refs),
	}
	for _, ref := range refs {
		if strings.TrimSpace(ref.WorkflowID) == "" {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("bill_id=%s missing workflow_id", ref.BillID.String()))
			continue
		}

		runID, err := s.describeBillingWorkflowRunID(ctx, ref.WorkflowID)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("bill_id=%s workflow_id=%s describe_error=%v", ref.BillID.String(), ref.WorkflowID, err))
			continue
		}
		if err := setBillWorkflowRunID(ctx, ref.BillID, runID); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("bill_id=%s workflow_id=%s run_id=%s persist_error=%v", ref.BillID.String(), ref.WorkflowID, runID, err))
			continue
		}
		res.Backfilled++
	}
	log.Printf("workflow run id reconciliation scanned=%d backfilled=%d failed=%d", res.Scanned, res.Backfilled, res.Failed)
	return res, nil
}
