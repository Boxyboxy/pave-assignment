# Sentinel Billing API (Encore + Temporal)

A billing service that accrues fees progressively during a billing period. All writes (add line item, close bill) are routed through Temporal for durable execution, while reads (get/list bills) hit the database directly for low latency.

## Project structure

```text
/pave-assignment
├── encore.app
├── billing/
│   ├── migrations/
│   │   └── 1_init_schema.up.sql
│   ├── handlers.go          # HTTP handlers + request/response types
│   ├── validation.go        # Request validation, parsing, payload hashing
│   ├── converters.go        # Workflow ↔ API type conversion + Temporal error mapping
│   ├── temporal_client.go   # Service definition + Temporal client (start, update, signal, wait)
│   ├── store.go             # Database reads + bill creation
│   ├── types.go             # Domain types (Bill, LineItem, Currency)
│   ├── api_validation_test.go
│   ├── workflow_test.go
│   └── workflowdef/
│       ├── workflow.go      # Temporal workflow (Update handler, Signal, timer)
│       ├── activities.go    # Temporal activities (PersistLineItem, MarkClosed, SendInvoice)
│       └── types.go         # Shared types, payloads, sentinel errors
├── internal/workerdb/
│   ├── query.sql            # sqlc query definitions (source of truth)
│   ├── db.go                # sqlc generated: DBTX interface, Queries struct
│   ├── models.go            # sqlc generated: table models
│   └── query.sql.go         # sqlc generated: query functions
├── cmd/fees-worker/
│   ├── main.go              # Worker bootstrap + wiring
│   ├── close_bill.go        # CloseBill activity implementation
│   ├── persist_line_item.go # PersistLineItem activity + invoice stub
│   └── main_test.go
├── sqlc.yaml                # sqlc configuration
├── scripts/
│   └── test_endpoints.sh    # Automated e2e endpoint test
└── docs/
    └── API.md               # Full API reference
```

## Design decisions

- **Money representation**: strongly-typed integer minor units (`MinorUnit`) backing `amount_minor` and `total_minor` to avoid floating-point errors and make monetary values harder to mix up with unrelated integers.
- **Currencies**: `USD`, `GEL`.
- **Lifecycle**: `OPEN -> CLOSED` (normal path) or `OPEN -> CANCELLED` (failed creation compensation). No reopen in MVP.
- **Entity lifecycle & data model**: a `Bill` is created with an account, currency, and period (`period_start` = now, `period_end` from caller). While `status=OPEN`, callers may append immutable `LineItem`s via the Temporal workflow. On close (manual or timer-based) the workflow marks the bill `CLOSED`, freezes the total, and records `closed_at`; failed create flows transition the bill to `CANCELLED` instead of deleting records.
- **Idempotency**: `idempotency_key` is required for line-item writes; `(bill_id, idempotency_key)` unique constraint prevents duplicate charges on retries. Payload hash detects accidental reuse with different data.
- **Temporal as the write path**: all mutating operations (add line item, close bill) are routed through Temporal, making the workflow the single owner of the bill lifecycle. This guarantees retries, consistency, and crash resilience for every write. Pure DB reads (get bill, list bills) bypass Temporal for low latency.
- **Temporal Update for line items**: `AddLineItem` uses the Temporal Workflow Update API (`SetUpdateHandlerWithOptions`) to send requests into the running workflow and receive synchronous responses — no client polling required. A validator rejects items if the bill is already closed, and the workflow maintains a running total in memory.
- **Temporal Signal for close**: `CloseBill` sends a signal to the running workflow, which then executes close activities (mark closed in DB, send invoice). If the workflow has already completed (auto-close via timer), a fallback workflow is started with `period_end = now` to guarantee closure.
- **Worker architecture**: the Temporal worker connects directly to the database using [sqlc](https://sqlc.dev)-generated type-safe queries (`internal/workerdb/`), avoiding a circular HTTP callback to the API. Connection pool tuning and query timeouts are enforced.
- **Soft cancellation**: failed bill creation is compensated with a `CANCELLED` status transition (no hard deletes of financial records).
- **Create-flow hardening**: if workflow starts but `workflow_run_id` persistence fails, service attempts compensating workflow termination and logs structured failure context with severity levels (`CRITICAL`/`WARNING`).
- **Reconciliation support**: private endpoint can backfill missing `workflow_run_id` values using Temporal describe.
- **Concurrency semantics**: the workflow processes updates sequentially (single-threaded workflow execution), serializing all writes per bill. The close-bill activity uses `SELECT ... FOR UPDATE` row locking for safe DB state transitions.
- **Audit trail**: `updated_at` timestamp tracks the last modification time for every bill mutation.
- **Clock consistency**: system timestamps (`created_at`, `updated_at`, `closed_at`) are generated by Postgres (`NOW()`), while domain timestamps (`period_start`, `period_end`) are supplied by the application or client. This keeps system time authoritative in the database while allowing callers to control billing periods.
- **Close semantics**: the `PATCH /bills/:id` close operation waits up to a small, fixed budget (2s) for the underlying Temporal workflow to finish marking the bill closed and sending the invoice. If that wait times out or fails, the API returns an error instead of pretending the bill is closed, so callers can treat the bill as “processing” and retry. This choice is acceptable here because we are not waiting on long-running 3rd-party processes that would routinely exceed that budget.
- **Auto-accrual support**: recurring or time-based fees (subscriptions, interest accrual, etc.) can be implemented inside the Temporal workflow using timers and periodic activities. This removes the need for ad-hoc cron jobs or custom orchestration glue outside the service; the workflow itself becomes the orchestrator for when and how to add new line items over the billing period.

## High-level architecture

```text
                    +----------------------+
                    |   External Clients   |
                    |  (other services,    |
                    |   curl, UI, etc.)    |
                    +----------+-----------+
                               |
                               |  HTTP/JSON
                               v
                 +-------------+--------------+
                 |      Encore Billing        |
                 |      Service (API)         |
                 |----------------------------|
                 | - POST /bills              |
                 | - POST /bills/:id/items    |
                 | - PATCH /bills/:id         |
                 | - GET  /bills/:id          |
                 | - GET  /bills              |
                 +------+------+--------------+
                        |      |
     (writes via        |      | (reads)
      Temporal)         |      v
                        |   +-----------------+
                        |   |  Postgres DB    |
                        |   |  (bills,        |
                        |   |   line_items)   |
                        |   +-----------------+
                        |
                        |  Temporal SDK
                        v
           +------------+-----------------+
           |       Temporal Server        |
           |    (Workflows & History)     |
           +------------+-----------------+
                        |
                        |  Task Queue: "billing-workflow"
                        v
       +----------------+----------------------+
       |           Temporal Worker             |
       |        (cmd/fees-worker)              |
       |---------------------------------------|
       |  - Executes BillingWorkflow           |
       |  - Activities:                        |
       |      * PersistLineItem (DB write)     |
       |      * MarkClosedInDB (DB write)      |
       |      * SendInvoiceEmail (stub)        |
       +----------------+----------------------+
                        |
                        |  SQL (via sqlc)
                        v
                    +-----------------+
                    |  Postgres DB    |
                    +-----------------+
```

## API endpoints

- `POST /bills` — create bill and start Temporal workflow (requires `account_id`, `currency`, `period_end`)
- `POST /bills/:billID/items` — add line item via Temporal Update (requires `idempotency_key`; synchronous response)
- `PATCH /bills/:billID` — update bill status (currently only supports `{"status":"CLOSED"}`; waits briefly for workflow completion)
- `GET /bills/:billID` — get bill with items (direct DB read)
- `GET /bills?status=OPEN|CLOSED|CANCELLED&account_id=...&limit=50&offset=0` — list bills, paginated (direct DB read)
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

CREATE_RESP="$(curl -sS -X POST "$BASE_URL/bills" \
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

## Regenerating sqlc

If you modify `internal/workerdb/query.sql` or the migration schema, regenerate the Go code:

```bash
sqlc generate
```

Generated files: `internal/workerdb/db.go`, `models.go`, `query.sql.go`.

## Running tests

```bash
# Billing service tests (requires Encore runtime for DB access)
encore test ./billing/... -v

# Worker tests (pure Go, no Encore runtime needed)
go test ./cmd/fees-worker/... -v
```
