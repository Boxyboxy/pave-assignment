package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"pave-assignment/billing/workflowdef"

	"encore.dev/beta/errs"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
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

//encore:api public method=POST path=/bills
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
// Add line item (write delegated to Temporal workflow via Update)
// ---------------------------------------------------------------------------

//encore:api public method=POST path=/bills/:billID/items
func (s *Service) AddLineItem(ctx context.Context, billID string, req *AddLineItemRequest) (*AddLineItemResponse, error) {
	id, payloadHash, err := validateAddLineItemRequest(billID, req)
	if err != nil {
		return nil, errs.B().Code(errs.InvalidArgument).Msg(err.Error()).Err()
	}

	workflowID := "bill-" + id.String()
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

type AddLineItemRequest struct {
	Description    string    `json:"description"`
	AmountMinor    MinorUnit `json:"amount_minor"`
	IdempotencyKey string    `json:"idempotency_key"`
}

type AddLineItemResponse struct {
	Bill Bill     `json:"bill"`
	Item LineItem `json:"item"`
}

// ---------------------------------------------------------------------------
// Close bill (write delegated to Temporal workflow via Signal)
// ---------------------------------------------------------------------------

type UpdateBillRequest struct {
	Status string `json:"status"`
}

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

	workflowID := "bill-" + id.String()

	// Signal the running workflow to close. The workflow loop will break,
	// execute MarkClosedInDB + SendInvoice activities, then complete.
	signalErr := s.signalCloseBillingWorkflow(ctx, workflowID)
	if signalErr == nil {
		// Signal accepted — wait for workflow to finish (close + invoice),
		// but enforce a short upper bound on how long we block the caller.
		if err := s.waitForWorkflowWithTimeout(ctx, workflowID, 2*time.Second); err != nil {
			return nil, err
		}
	} else {
		log.Printf("WARNING: signal close failed bill_id=%s err=%v (checking DB state)", billID, signalErr)

		// Workflow may have already completed (timer auto-close). Check DB.
		b, err := getBill(ctx, id)
		if err != nil {
			return nil, err
		}
		if string(b.Status) == string(BillStatusOpen) {
			// Bill is still OPEN but workflow is unreachable — start a
			// fallback workflow with period_end = now so it closes immediately
			// via the MarkClosedInDB activity. Deterministic workflow ID
			// makes this idempotent across retries.
			fallbackID := "bill-close-fallback-" + id.String()
			if _, fbErr := s.startBillingWorkflow(ctx, fallbackID, workflowdef.BillingWorkflowInput{
				BillID:    id.String(),
				PeriodEnd: time.Now().UTC(),
			}); fbErr != nil {
				log.Printf("WARNING: fallback workflow start failed bill_id=%s err=%v", billID, fbErr)
				return nil, errs.B().Code(errs.Unavailable).
					Msg("unable to close bill: workflow service unavailable").Err()
			}
			if err := s.waitForWorkflowWithTimeout(ctx, fallbackID, 2*time.Second); err != nil {
				return nil, err
			}
		} else {
			// Already CLOSED or CANCELLED, fall through to DB read below.
		}
	}

	// Pure DB read for the response. At this point we either:
	// - Observed the workflow complete within our wait window, or
	// - Determined the bill was already CLOSED/CANCELLED.
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
// Get bill
// ---------------------------------------------------------------------------

//encore:api public method=GET path=/bills/:billID
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
	return id, computeLineItemPayloadHash(req.Description, int64(req.AmountMinor)), nil
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

// ---------------------------------------------------------------------------
// Temporal error mapping
// ---------------------------------------------------------------------------

// mapTemporalError converts Temporal-domain errors into Encore API errors
// with the appropriate gRPC status code.
func mapTemporalError(err error) error {
	if err == nil {
		return nil
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		switch appErr.Type() {
		case "FAILED_PRECONDITION":
			return errs.B().Code(errs.FailedPrecondition).Msg(appErr.Message()).Err()
		case "ALREADY_EXISTS":
			return errs.B().Code(errs.AlreadyExists).Msg(appErr.Message()).Err()
		case "NOT_FOUND":
			return errs.B().Code(errs.NotFound).Msg(appErr.Message()).Err()
		}
	}
	// For unexpected errors, propagate a trimmed version of the original
	// message so callers have some context without leaking excessive detail.
	return errs.B().
		Code(errs.Internal).
		Msg(fmt.Sprintf("add line item failed: %v", err)).
		Err()
}

// ---------------------------------------------------------------------------
// Workflow ↔ API type conversion
// ---------------------------------------------------------------------------

func wfBillToBill(b workflowdef.WfBill) Bill {
	return Bill{
		ID:          b.ID,
		AccountID:   b.AccountID,
		Currency:    Currency(b.Currency),
		Status:      BillStatus(b.Status),
		CreatedAt:   b.CreatedAt,
		UpdatedAt:   b.UpdatedAt,
		PeriodStart: b.PeriodStart,
		PeriodEnd:   b.PeriodEnd,
		ClosedAt:    b.ClosedAt,
		TotalMinor:  MinorUnit(b.TotalMinor),
	}
}

func wfLineItemToLineItem(li workflowdef.WfLineItem) LineItem {
	return LineItem{
		ID:             li.ID,
		BillID:         li.BillID,
		IdempotencyKey: li.IdempotencyKey,
		Description:    li.Description,
		AmountMinor:    MinorUnit(li.AmountMinor),
		CreatedAt:      li.CreatedAt,
	}
}
