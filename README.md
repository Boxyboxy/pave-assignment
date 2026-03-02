# Sentinel Billing API (Encore + Temporal)

A billing service that accrues fees progressively during a billing period and closes bills via Temporal workflow orchestration.

## Project structure

```text
/pave-assignment
├── encore.app
├── billing/
│   ├── migrations/
│   │   ├── 1_init_schema.up.sql
│   │   └── 1_init_schema.down.sql
│   ├── api.go              # HTTP handlers (Service methods)
│   ├── store.go             # Database access layer
│   ├── workflow.go          # Temporal client helpers
│   ├── types.go             # Domain types (Bill, LineItem, Currency)
│   ├── api_validation_test.go
│   ├── workflow_test.go
│   └── workflowdef/
│       ├── workflow.go      # Temporal workflow definition
│       ├── activities.go    # Temporal activity implementations
│       └── closebill.go     # Shared close-bill SQL & BillCloser interface
├── cmd/fees-worker/
│   ├── main.go              # Temporal worker binary
│   └── main_test.go
├── scripts/
│   └── test_endpoints.sh    # Automated e2e endpoint test
└── docs/
    └── API.md               # Full API reference
```

## Design decisions

- **Money representation**: integer minor units (`amount_minor`, `total_minor`) to avoid floating-point errors.
- **Currencies**: `USD`, `GEL`.
- **Lifecycle**: `OPEN -> CLOSED` (normal path) or `OPEN -> CANCELLED` (failed creation compensation). No reopen in MVP.
- **Idempotency**: `idempotency_key` is required for line-item writes; `(bill_id, idempotency_key)` unique constraint prevents duplicate charges on retries. Payload hash detects accidental reuse with different data.
- **Temporal role**: durable timer/signal-driven close at period end (or manual close signal), resilient to process restarts.
- **Worker architecture**: the Temporal worker connects directly to the database to close bills, avoiding a circular HTTP callback to the API.
- **Soft cancellation**: failed bill creation is compensated with a `CANCELLED` status transition (no hard deletes of financial records).
- **Create-flow hardening**: if workflow starts but `workflow_run_id` persistence fails, service attempts compensating workflow termination and logs structured failure context with severity levels (`CRITICAL`/`WARNING`).
- **Reconciliation support**: private endpoint can backfill missing `workflow_run_id` values using Temporal describe.
- **Concurrency semantics**: both close and addLineItem lock the bill row (`SELECT ... FOR UPDATE`) so transitions serialize per bill, preventing race conditions between concurrent close and add operations.
- **Audit trail**: `updated_at` timestamp tracks the last modification time for every bill mutation.

## API endpoints

- `POST /bill` — create bill (requires `account_id`, `currency`, `period_end`)
- `POST /bill/:billID/item` — add line item (requires `idempotency_key`)
- `POST /bill/:billID/close` — close bill (idempotent; signals workflow + DB close with row lock)
- `GET /bill/:billID` — get bill with items
- `GET /bill?status=OPEN|CLOSED|CANCELLED&account_id=...&limit=50&offset=0` — list bills (paginated)
- `POST /internal/workflows/reconcile-run-ids` — private run-id reconciliation endpoint

Full API reference with request/response examples: `docs/API.md`.

## Prerequisites

Before running locally, make sure these are done:

- Docker daemon is running (Encore uses Docker to start local PostgreSQL for `sqldb`).
- Encore CLI is installed and authenticated (`encore auth login`).
- This repository is linked to a real Encore app id (not a placeholder id).

Helpful checks:

```bash
docker ps
encore auth whoami
cat encore.app
```

If app linking is needed:

```bash
encore app link --force <your-app-id>
```

## Local setup

### 1) Set repo-local Go proxy env

```bash
source scripts/use-local-go-proxy.sh
```

### 2) Install dependencies

```bash
go mod tidy
```

### 3) Start services (3 terminals, in this order)

Terminal 1 — Temporal server:

```bash
source scripts/use-local-go-proxy.sh
temporal server start-dev
```

Terminal 2 — Encore API:

```bash
source scripts/use-local-go-proxy.sh
encore run
```

Terminal 3 — Temporal worker:

```bash
source scripts/use-local-go-proxy.sh
DATABASE_URL="$(encore db conn-uri billing)" TEMPORAL_ADDRESS=127.0.0.1:7233 go run ./cmd/fees-worker
```

Notes:
- Start in order: Temporal -> Encore API -> worker.
- Always run from repo root, not any nested directory.
- The worker connects directly to the database (no HTTP callback to the API).

## Quick e2e test

```bash
BASE_URL="http://127.0.0.1:4000"
PERIOD_END="$(date -u -v+15M +%Y-%m-%dT%H:%M:%SZ)"

CREATE_RESP="$(curl -sS -X POST "$BASE_URL/bill" \
  -H "Content-Type: application/json" \
  -d "{\"account_id\":\"acct-demo\",\"currency\":\"USD\",\"period_end\":\"$PERIOD_END\"}")"
echo "$CREATE_RESP"
```

Then continue with add/get/close/list examples from `docs/API.md`.

## Automated endpoint script

Once Temporal, Encore API, and worker are running, execute:

```bash
./scripts/test_endpoints.sh
```

Optional custom base URL:

```bash
BASE_URL="http://127.0.0.1:4000" ./scripts/test_endpoints.sh
```

## Running tests

```bash
# Billing service tests (requires Encore runtime for DB access)
encore test ./billing/... -v

# Worker tests (pure Go, no Encore runtime needed)
go test ./cmd/fees-worker/... -v
```
