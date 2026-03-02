#!/usr/bin/env bash
set -euo pipefail

# End-to-end endpoint test for local development.
# Prerequisites:
# - temporal server start-dev
# - encore run
# - go run ./cmd/fees-worker

BASE_URL="${BASE_URL:-http://127.0.0.1:4000}"

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required for this script. Install jq and retry."
  exit 1
fi

echo "Using BASE_URL=${BASE_URL}"

SHORT_END="$(python3 - <<'PY'
from datetime import datetime, timezone, timedelta
print((datetime.now(timezone.utc)+timedelta(seconds=10)).strftime("%Y-%m-%dT%H:%M:%SZ"))
PY
)"

echo "1) Create bill"
CREATE_RESP="$(curl -sS -X POST "$BASE_URL/bill" \
  -H "Content-Type: application/json" \
  -d "{\"account_id\":\"acct-test-1\",\"currency\":\"USD\",\"period_end\":\"$SHORT_END\"}")"
echo "$CREATE_RESP" | jq .

BILL_ID="$(echo "$CREATE_RESP" | jq -r '.id')"
if [[ -z "$BILL_ID" || "$BILL_ID" == "null" ]]; then
  echo "Create bill failed: missing id"
  exit 1
fi

# Verify updated_at is present on created bill
UPDATED_AT="$(echo "$CREATE_RESP" | jq -r '.updated_at')"
if [[ -z "$UPDATED_AT" || "$UPDATED_AT" == "null" ]]; then
  echo "Create bill response missing updated_at"
  exit 1
fi

echo "2) Add line item"
ADD_RESP="$(curl -sS -X POST "$BASE_URL/bill/$BILL_ID/item" \
  -H "Content-Type: application/json" \
  -d '{"description":"quick fee","amount_minor":123,"idempotency_key":"quick-1"}')"
echo "$ADD_RESP" | jq .

TOTAL_NOW="$(echo "$ADD_RESP" | jq -r '.bill.total_minor')"
if [[ "$TOTAL_NOW" != "123" ]]; then
  echo "Unexpected total after add: $TOTAL_NOW"
  exit 1
fi

# Verify bill.updated_at advanced after line item was added
UPDATED_AFTER_ADD="$(echo "$ADD_RESP" | jq -r '.bill.updated_at')"
if [[ -z "$UPDATED_AFTER_ADD" || "$UPDATED_AFTER_ADD" == "null" ]]; then
  echo "Add line item response missing bill.updated_at"
  exit 1
fi

echo "3) Idempotency check (same key should not increase total)"
ADD_DUP_RESP="$(curl -sS -X POST "$BASE_URL/bill/$BILL_ID/item" \
  -H "Content-Type: application/json" \
  -d '{"description":"quick fee","amount_minor":123,"idempotency_key":"quick-1"}')"
echo "$ADD_DUP_RESP" | jq .
TOTAL_DUP="$(echo "$ADD_DUP_RESP" | jq -r '.bill.total_minor')"
if [[ "$TOTAL_DUP" != "123" ]]; then
  echo "Idempotency failed: total became $TOTAL_DUP"
  exit 1
fi

echo "4) Payload mismatch check (same key + different payload should fail)"
MISMATCH_HTTP_CODE="$(curl -sS -o /tmp/billing_idem_mismatch_resp.json -w "%{http_code}" \
  -X POST "$BASE_URL/bill/$BILL_ID/item" \
  -H "Content-Type: application/json" \
  -d '{"description":"quick fee changed","amount_minor":999,"idempotency_key":"quick-1"}')"
cat /tmp/billing_idem_mismatch_resp.json | jq .
if [[ "$MISMATCH_HTTP_CODE" -lt 400 ]]; then
  echo "Expected idempotency mismatch failure, got HTTP ${MISMATCH_HTTP_CODE}"
  exit 1
fi

echo "5) Missing idempotency key should fail"
MISSING_IDEM_HTTP_CODE="$(curl -sS -o /tmp/billing_missing_idem_resp.json -w "%{http_code}" \
  -X POST "$BASE_URL/bill/$BILL_ID/item" \
  -H "Content-Type: application/json" \
  -d '{"description":"missing key","amount_minor":50}')"
cat /tmp/billing_missing_idem_resp.json | jq .
if [[ "$MISSING_IDEM_HTTP_CODE" -lt 400 ]]; then
  echo "Expected missing idempotency key failure, got HTTP ${MISSING_IDEM_HTTP_CODE}"
  exit 1
fi

echo "6) Poll until auto-closed by workflow (up to 30s)"
STATUS=""
for _ in {1..15}; do
  BILL_RESP="$(curl -sS "$BASE_URL/bill/$BILL_ID")"
  STATUS="$(echo "$BILL_RESP" | jq -r '.bill.status')"
  echo "status=${STATUS}"
  if [[ "$STATUS" == "CLOSED" ]]; then
    break
  fi
  sleep 2
done

if [[ "$STATUS" != "CLOSED" ]]; then
  echo "Bill did not auto-close in expected time window"
  exit 1
fi

# Verify closed_at and updated_at are populated after close
CLOSED_AT="$(echo "$BILL_RESP" | jq -r '.bill.closed_at')"
if [[ -z "$CLOSED_AT" || "$CLOSED_AT" == "null" ]]; then
  echo "Bill missing closed_at after auto-close"
  exit 1
fi

echo "7) Verify adding item to closed bill is rejected"
HTTP_CODE="$(curl -sS -o /tmp/billing_closed_add_resp.json -w "%{http_code}" \
  -X POST "$BASE_URL/bill/$BILL_ID/item" \
  -H "Content-Type: application/json" \
  -d '{"description":"late fee","amount_minor":100,"idempotency_key":"late-1"}')"
cat /tmp/billing_closed_add_resp.json | jq .
if [[ "$HTTP_CODE" -lt 400 ]]; then
  echo "Expected failure when adding to closed bill, got HTTP ${HTTP_CODE}"
  exit 1
fi

echo "8) Manual close a second bill (verify close endpoint + updated_at)"
MANUAL_END="$(python3 - <<'PY'
from datetime import datetime, timezone, timedelta
print((datetime.now(timezone.utc)+timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ"))
PY
)"
MANUAL_CREATE="$(curl -sS -X POST "$BASE_URL/bill" \
  -H "Content-Type: application/json" \
  -d "{\"account_id\":\"acct-test-1\",\"currency\":\"GEL\",\"period_end\":\"$MANUAL_END\"}")"
echo "$MANUAL_CREATE" | jq .
MANUAL_ID="$(echo "$MANUAL_CREATE" | jq -r '.id')"

curl -sS -X POST "$BASE_URL/bill/$MANUAL_ID/item" \
  -H "Content-Type: application/json" \
  -d '{"description":"gel fee","amount_minor":500,"idempotency_key":"gel-1"}' | jq .

CLOSE_RESP="$(curl -sS -X POST "$BASE_URL/bill/$MANUAL_ID/close")"
echo "$CLOSE_RESP" | jq .
CLOSE_STATUS="$(echo "$CLOSE_RESP" | jq -r '.bill.status')"
if [[ "$CLOSE_STATUS" != "CLOSED" ]]; then
  echo "Expected CLOSED status after manual close, got ${CLOSE_STATUS}"
  exit 1
fi
CLOSE_UPDATED="$(echo "$CLOSE_RESP" | jq -r '.bill.updated_at')"
if [[ -z "$CLOSE_UPDATED" || "$CLOSE_UPDATED" == "null" ]]; then
  echo "Close response missing bill.updated_at"
  exit 1
fi

echo "9) List closed bills with pagination and ensure both bills are present"
LIST_RESP="$(curl -sS "$BASE_URL/bill?status=CLOSED&account_id=acct-test-1&limit=10&offset=0")"
echo "$LIST_RESP" | jq .
MATCHED="$(echo "$LIST_RESP" | jq -r --arg id "$BILL_ID" '.bills[]? | select(.id == $id) | .id' | head -n1)"
if [[ "$MATCHED" != "$BILL_ID" ]]; then
  echo "Closed bills list does not include auto-closed bill"
  exit 1
fi
TOTAL="$(echo "$LIST_RESP" | jq -r '.total')"
if [[ "$TOTAL" -lt 2 ]]; then
  echo "Expected total >= 2, got $TOTAL"
  exit 1
fi

echo "All endpoint checks passed."
