package workflowdef

import "errors"

// Sentinel errors used by activities to signal non-retryable business conditions.
// These are matched by the API layer (via Temporal ApplicationError type codes)
// to return the appropriate gRPC/HTTP status codes to callers.
var (
	ErrBillNotFound    = errors.New("bill not found")
	ErrBillNotOpen     = errors.New("bill is not open")
	ErrPayloadMismatch = errors.New("idempotency_key already used with different payload")
)
