package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"pave-assignment/billing/workflowdef"

	"encore.dev/beta/errs"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
)

// ---------------------------------------------------------------------------
// Service definition
// ---------------------------------------------------------------------------

//encore:service
type Service struct {
	// tc is the lazily-initialised Temporal client. Access via getTemporalClient().
	tc   client.Client
	tcMu sync.Mutex
}

func initService() (*Service, error) {
	return &Service{}, nil
}

// ---------------------------------------------------------------------------
// Create bill
// ---------------------------------------------------------------------------

//encore:api public method=POST path=/bill
func (s *Service) CreateBill(ctx context.Context, req *CreateBillRequest) (*Bill, error) {
	now := time.Now().UTC()
	periodEnd, err := validateCreateBillRequest(req, now)
	if err != nil {
		return nil, errs.B().Code(errs.InvalidArgument).Msg(err.Error()).Err()
	}

	billID := uuid.New()
	workflowID := "bill-" + billID.String()

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

type CreateBillRequest struct {
	AccountID string    `json:"account_id"`
	Currency  Currency  `json:"currency"`
	PeriodEnd time.Time `json:"period_end"`
}

// ---------------------------------------------------------------------------
// Add line item
// ---------------------------------------------------------------------------

//encore:api public method=POST path=/bill/:billID/item
func (s *Service) AddLineItem(ctx context.Context, billID string, req *AddLineItemRequest) (*AddLineItemResponse, error) {
	id, payloadHash, err := validateAddLineItemRequest(billID, req)
	if err != nil {
		return nil, errs.B().Code(errs.InvalidArgument).Msg(err.Error()).Err()
	}
	res, err := addLineItem(ctx, addLineItemParams{
		BillID:         id,
		Description:    req.Description,
		AmountMinor:    req.AmountMinor,
		IdempotencyKey: req.IdempotencyKey,
		PayloadHash:    payloadHash,
	})
	if err != nil {
		return nil, err
	}
	return &AddLineItemResponse{Bill: res.Bill, Item: res.Item}, nil
}

type AddLineItemRequest struct {
	Description    string `json:"description"`
	AmountMinor    int64  `json:"amount_minor"`
	IdempotencyKey string `json:"idempotency_key"`
}

type AddLineItemResponse struct {
	Bill Bill     `json:"bill"`
	Item LineItem `json:"item"`
}

// ---------------------------------------------------------------------------
// Close bill
// ---------------------------------------------------------------------------

//encore:api public method=POST path=/bill/:billID/close
func (s *Service) CloseBill(ctx context.Context, billID string) (*BillWithItems, error) {
	id, err := parseBillID(billID)
	if err != nil {
		return nil, errs.B().Code(errs.InvalidArgument).Msg(err.Error()).Err()
	}

	// Attempt to signal the existing workflow for a clean close.
	// If the signal succeeds, the workflow will close the bill and send the invoice.
	signalSent := false
	if workflowID, wfErr := getBillWorkflowID(ctx, id); wfErr == nil {
		if sigErr := s.signalCloseBillingWorkflow(ctx, workflowID); sigErr != nil {
			log.Printf(
				"WARNING: failed to signal workflow close bill_id=%s workflow_id=%s err=%v",
				billID, workflowID, sigErr,
			)
		} else {
			signalSent = true
		}
	}

	// Close bill in DB (transactional with row lock for serialization).
	b, err := closeBill(ctx, id)
	if err != nil {
		return nil, err
	}

	// If the workflow signal failed (workflow already completed / terminated /
	// unreachable), start a fallback workflow to guarantee invoice delivery.
	// The fallback has period_end = now so it fires immediately:
	//   1. MarkClosedInDB → no-op (bill already closed)
	//   2. SendInvoiceEmail → delivers the invoice
	// Using a deterministic workflow ID ("bill-close-fallback-<id>") makes
	// this idempotent: Temporal rejects duplicate IDs, so retrying the API
	// call won't start multiple fallback workflows.
	if !signalSent {
		fallbackID := "bill-close-fallback-" + id.String()
		if _, fbErr := s.startBillingWorkflow(ctx, fallbackID, workflowdef.BillingWorkflowInput{
			BillID:    id.String(),
			PeriodEnd: time.Now().UTC(),
		}); fbErr != nil {
			log.Printf(
				"WARNING: failed to start fallback invoice workflow bill_id=%s fallback_workflow_id=%s err=%v",
				billID, fallbackID, fbErr,
			)
		}
	}

	items, err := listLineItems(ctx, id)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []LineItem{}
	}
	return &BillWithItems{Bill: *b, Items: items}, nil
}

// ---------------------------------------------------------------------------
// Get bill
// ---------------------------------------------------------------------------

//encore:api public method=GET path=/bill/:billID
func (s *Service) GetBill(ctx context.Context, billID string) (*BillWithItems, error) {
	id, err := parseBillID(billID)
	if err != nil {
		return nil, errs.B().Code(errs.InvalidArgument).Msg(err.Error()).Err()
	}
	b, err := getBill(ctx, id)
	if err != nil {
		return nil, err
	}
	items, err := listLineItems(ctx, id)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []LineItem{}
	}
	return &BillWithItems{Bill: *b, Items: items}, nil
}

// ---------------------------------------------------------------------------
// List bills (paginated)
// ---------------------------------------------------------------------------

//encore:api public method=GET path=/bill
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

type ListBillsRequest struct {
	Status    string `query:"status"`
	AccountID string `query:"account_id"`
	Limit     int    `query:"limit"`
	Offset    int    `query:"offset"`
}

type ListBillsResponse struct {
	Bills  []Bill `json:"bills"`
	Total  int    `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

// ---------------------------------------------------------------------------
// Reconcile workflow run IDs (private / operational)
// ---------------------------------------------------------------------------

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

type ReconcileWorkflowRunIDsRequest struct {
	Limit int `json:"limit"`
}

type ReconcileWorkflowRunIDsResponse struct {
	Scanned    int      `json:"scanned"`
	Backfilled int      `json:"backfilled"`
	Failed     int      `json:"failed"`
	Errors     []string `json:"errors,omitempty"`
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

func validateCreateBillRequest(req *CreateBillRequest, now time.Time) (time.Time, error) {
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

func validateAddLineItemRequest(billID string, req *AddLineItemRequest) (uuid.UUID, string, error) {
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
	return id, computeLineItemPayloadHash(req.Description, req.AmountMinor), nil
}

func computeLineItemPayloadHash(description string, amountMinor int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", description, amountMinor)))
	return hex.EncodeToString(sum[:])
}

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

func parseBillID(billID string) (uuid.UUID, error) {
	id, err := uuid.Parse(billID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid bill_id")
	}
	return id, nil
}
