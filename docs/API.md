# Billing API Reference

Base URL (local): `http://127.0.0.1:4000`

All amounts are represented in integer minor units:
- USD: cents
- GEL: tetri

---

## 1) Create bill

`POST /bills`

### Request body

```json
{
  "account_id": "acct-123",
  "currency": "USD",
  "period_end": "2026-03-01T17:45:00Z"
}
```

| Field        | Type   | Required | Notes                                           |
|--------------|--------|----------|-------------------------------------------------|
| `account_id` | string | yes      | Identifies the account that owns this bill      |
| `currency`   | string | yes      | `"USD"` or `"GEL"`                              |
| `period_end` | string | yes      | RFC 3339 timestamp; must be in the future       |

### Response (200)

```json
{
  "id": "35191f9f-ca9b-4cb8-91d8-aeec0b826b4d",
  "account_id": "acct-123",
  "currency": "USD",
  "status": "OPEN",
  "created_at": "2026-03-01T17:30:00Z",
  "updated_at": "2026-03-01T17:30:00Z",
  "period_start": "2026-03-01T17:30:00Z",
  "period_end": "2026-03-01T17:45:00Z",
  "total_minor": 0
}
```

### Common errors
- `invalid_argument` if:
  - `account_id` is missing or too long (>200 chars)
  - unsupported currency
  - missing/invalid `period_end`
  - `period_end` not in the future
  - `period_end` more than 365 days in the future
- `internal` if workflow started but create flow could not be finalized (service attempts compensating workflow termination and logs at `CRITICAL` severity)

### Compensation behavior
- If the Temporal workflow fails to start after the bill is created, the bill is soft-cancelled (`CANCELLED` status) instead of hard-deleted. This preserves an audit trail of all financial record creation attempts.
- If `workflow_run_id` persistence fails after workflow start, the service attempts to terminate the orphaned workflow and logs at `CRITICAL` or `WARNING` severity depending on whether termination also fails.

---

## 2) Add line item

`POST /bills/:billID/items`

`idempotency_key` is required for this endpoint.

### Request body

```json
{
  "description": "monthly fee",
  "amount_minor": 1299,
  "idempotency_key": "line-1"
}
```

| Field             | Type    | Required | Notes                                                   |
|-------------------|---------|----------|---------------------------------------------------------|
| `description`     | string  | yes      | Non-empty description of the charge                     |
| `amount_minor`    | integer | yes      | Must be > 0; in minor units (cents / tetri)             |
| `idempotency_key` | string  | yes      | Unique per-bill key; max 200 chars; prevents duplicates |

### Response (200)

```json
{
  "bill": {
    "id": "35191f9f-ca9b-4cb8-91d8-aeec0b826b4d",
    "account_id": "acct-123",
    "currency": "USD",
    "status": "OPEN",
    "created_at": "2026-03-01T17:30:00Z",
    "updated_at": "2026-03-01T17:31:00Z",
    "period_start": "2026-03-01T17:30:00Z",
    "period_end": "2026-03-01T17:45:00Z",
    "total_minor": 1299
  },
  "item": {
    "id": "ec57b8df-ec67-4f04-b98c-c57d36d2b68f",
    "bill_id": "35191f9f-ca9b-4cb8-91d8-aeec0b826b4d",
    "idempotency_key": "line-1",
    "description": "monthly fee",
    "amount_minor": 1299,
    "created_at": "2026-03-01T17:31:00Z"
  }
}
```

### Common errors
- `invalid_argument` for malformed bill id, empty description, non-positive amount, missing idempotency key, or too-long idempotency key (>200 chars)
- `failed_precondition` when bill is closed or cancelled
- `not_found` when bill does not exist
- `already_exists` when the same idempotency key is reused with a different payload (payload hash mismatch)

### Idempotency semantics
- Replaying the exact same request (same key + same payload) is safe and returns the original line item without changing the bill total.
- Sending a different payload with the same key is rejected with `already_exists`.

### Concurrency note
- Bill row is locked (`FOR UPDATE`) while adding a line item so close/add transitions serialize per bill.

---

## 3) Get bill details

`GET /bills/:billID`

### Response (200)

```json
{
  "bill": {
    "id": "35191f9f-ca9b-4cb8-91d8-aeec0b826b4d",
    "account_id": "acct-123",
    "currency": "USD",
    "status": "OPEN",
    "created_at": "2026-03-01T17:30:00Z",
    "updated_at": "2026-03-01T17:31:00Z",
    "period_start": "2026-03-01T17:30:00Z",
    "period_end": "2026-03-01T17:45:00Z",
    "total_minor": 1800
  },
  "items": [
    {
      "id": "ec57b8df-ec67-4f04-b98c-c57d36d2b68f",
      "bill_id": "35191f9f-ca9b-4cb8-91d8-aeec0b826b4d",
      "idempotency_key": "line-1",
      "description": "monthly fee",
      "amount_minor": 1299,
      "created_at": "2026-03-01T17:31:00Z"
    }
  ]
}
```

### Common errors
- `invalid_argument` for malformed bill id
- `not_found` when bill does not exist

---

## 4) Close bill

`PATCH /bills/:billID`

### Request body

```json
{
  "status": "CLOSED"
}
```

### Response (200)

Same shape as `GET /bill/:billID`, but `bill.status` is `CLOSED` and `bill.closed_at` is populated.

### Notes
- Endpoint is idempotent: repeated close calls return current closed state.
- Close uses a transactional row lock (`SELECT ... FOR UPDATE`) so it serializes cleanly with concurrent `addLineItem` calls on the same bill.
- Manual close also signals the Temporal workflow so it can terminate early. If the signal fails, a fallback workflow is started to ensure invoice delivery.
- The service waits up to ~2 seconds for the underlying Temporal workflow to complete the close + invoice activities. If that wait times out or fails, the API returns an `unavailable`/`internal` style error so clients can treat the bill as “processing” and retry, instead of assuming it is fully closed.

---

## 5) List bills (paginated)

`GET /bills`

### Query parameters

| Parameter    | Type    | Default | Notes                                    |
|--------------|---------|---------|------------------------------------------|
| `status`     | string  | (none)  | Filter: `OPEN`, `CLOSED`, or `CANCELLED` |
| `account_id` | string  | (none)  | Filter by account                        |
| `limit`      | integer | 50      | Max 200                                  |
| `offset`     | integer | 0       | Pagination offset                        |

### Response (200)

```json
{
  "bills": [
    {
      "id": "35191f9f-ca9b-4cb8-91d8-aeec0b826b4d",
      "account_id": "acct-123",
      "currency": "USD",
      "status": "CLOSED",
      "created_at": "2026-03-01T17:30:00Z",
      "updated_at": "2026-03-01T17:34:00Z",
      "period_start": "2026-03-01T17:30:00Z",
      "period_end": "2026-03-01T17:45:00Z",
      "closed_at": "2026-03-01T17:34:00Z",
      "total_minor": 1800
    }
  ],
  "total": 1,
  "limit": 50,
  "offset": 0
}
```

### Common errors
- `invalid_argument` if status is not `OPEN`, `CLOSED`, or `CANCELLED`

---

## 6) Reconcile missing workflow run ids (private)

`POST /internal/workflows/reconcile-run-ids`

Used operationally to backfill `workflow_run_id` for bills where workflow start succeeded but run-id persistence failed.

### Request body (optional)

```json
{
  "limit": 100
}
```

`limit` defaults to `100` and is capped at `500`.

### Response (200)

```json
{
  "scanned": 3,
  "backfilled": 2,
  "failed": 1,
  "errors": [
    "bill_id=... workflow_id=... describe_error=..."
  ]
}
```

### Notes
- Endpoint is private in Encore (`//encore:api private`), intended for internal operations/reconciliation jobs.
- A failure in one row does not stop processing of other rows in the batch.

---

## Copy-paste curl flow

```bash
BASE_URL="http://127.0.0.1:4000"
PERIOD_END="$(date -u -v+15M +%Y-%m-%dT%H:%M:%SZ)"

CREATE_RESP="$(curl -sS -X POST "$BASE_URL/bills" \
  -H "Content-Type: application/json" \
  -d "{\"account_id\":\"acct-demo\",\"currency\":\"USD\",\"period_end\":\"$PERIOD_END\"}")"

echo "$CREATE_RESP"
BILL_ID="$(echo "$CREATE_RESP" | jq -r '.id')"

curl -sS -X POST "$BASE_URL/bills/$BILL_ID/items" \
  -H "Content-Type: application/json" \
  -d '{"description":"monthly fee","amount_minor":1299,"idempotency_key":"line-1"}'

curl -sS -X POST "$BASE_URL/bills/$BILL_ID/items" \
  -H "Content-Type: application/json" \
  -d '{"description":"usage fee","amount_minor":501,"idempotency_key":"line-2"}'

curl -sS "$BASE_URL/bills/$BILL_ID"
curl -sS -X PATCH "$BASE_URL/bills/$BILL_ID" \
  -H "Content-Type: application/json" \
  -d '{"status":"CLOSED"}'
curl -sS "$BASE_URL/bills?status=CLOSED&account_id=acct-demo"
```
