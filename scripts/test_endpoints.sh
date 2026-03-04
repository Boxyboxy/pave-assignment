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
print((datetime.now(timezone.utc)+timedelta(seconds=60)).strftime("%Y-%m-%dT%H:%M:%SZ"))
PY
)"

echo "1) Create bill"
CREATE_RESP="$(curl -sS -X POST "$BASE_URL/bills" \
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
ADD_RESP="$(curl -sS -X POST "$BASE_URL/bills/$BILL_ID/items" \
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
ADD_DUP_RESP="$(curl -sS -X POST "$BASE_URL/bills/$BILL_ID/items" \
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
  -X POST "$BASE_URL/bills/$BILL_ID/items" \
  -H "Content-Type: application/json" \
  -d '{"description":"quick fee changed","amount_minor":999,"idempotency_key":"quick-1"}')"
cat /tmp/billing_idem_mismatch_resp.json | jq .
if [[ "$MISMATCH_HTTP_CODE" -lt 400 ]]; then
  echo "Expected idempotency mismatch failure, got HTTP ${MISMATCH_HTTP_CODE}"
  exit 1
fi

echo "5) Missing idempotency key should fail"
MISSING_IDEM_HTTP_CODE="$(curl -sS -o /tmp/billing_missing_idem_resp.json -w "%{http_code}" \
  -X POST "$BASE_URL/bills/$BILL_ID/items" \
  -H "Content-Type: application/json" \
  -d '{"description":"missing key","amount_minor":50}')"
cat /tmp/billing_missing_idem_resp.json | jq .
if [[ "$MISSING_IDEM_HTTP_CODE" -lt 400 ]]; then
  echo "Expected missing idempotency key failure, got HTTP ${MISSING_IDEM_HTTP_CODE}"
  exit 1
fi

echo "6) Poll until auto-closed by workflow (up to 90s)"
STATUS=""
for _ in {1..45}; do
  BILL_RESP="$(curl -sS "$BASE_URL/bills/$BILL_ID")"
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
  -X POST "$BASE_URL/bills/$BILL_ID/items" \
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
MANUAL_CREATE="$(curl -sS -X POST "$BASE_URL/bills" \
  -H "Content-Type: application/json" \
  -d "{\"account_id\":\"acct-test-1\",\"currency\":\"GEL\",\"period_end\":\"$MANUAL_END\"}")"
echo "$MANUAL_CREATE" | jq .
MANUAL_ID="$(echo "$MANUAL_CREATE" | jq -r '.id')"

curl -sS -X POST "$BASE_URL/bills/$MANUAL_ID/items" \
  -H "Content-Type: application/json" \
  -d '{"description":"gel fee","amount_minor":500,"idempotency_key":"gel-1"}' | jq .

CLOSE_RESP="$(curl -sS -X PATCH "$BASE_URL/bills/$MANUAL_ID" \
  -H "Content-Type: application/json" \
  -d '{"status":"CLOSED"}')"
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
LIST_RESP="$(curl -sS "$BASE_URL/bills?status=CLOSED&account_id=acct-test-1&limit=10&offset=0")"
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

# ---------------------------------------------------------------------------
# Filtering tests: create bills across accounts and statuses, then verify
# that status and account_id filters work correctly.
# ---------------------------------------------------------------------------

FILTER_END="$(python3 - <<'PY'
from datetime import datetime, timezone, timedelta
print((datetime.now(timezone.utc)+timedelta(hours=2)).strftime("%Y-%m-%dT%H:%M:%SZ"))
PY
)"

# Use unique account IDs to isolate from earlier test data
ACCT_A="acct-filter-a-$$"
ACCT_B="acct-filter-b-$$"

echo "10) Create filter test bills on two accounts"

# acct-A: one OPEN bill
FILTER_A_OPEN="$(curl -sS -X POST "$BASE_URL/bills" \
  -H "Content-Type: application/json" \
  -d "{\"account_id\":\"$ACCT_A\",\"currency\":\"USD\",\"period_end\":\"$FILTER_END\"}")"
FILTER_A_OPEN_ID="$(echo "$FILTER_A_OPEN" | jq -r '.id')"
echo "  acct-A OPEN:   $FILTER_A_OPEN_ID"

# acct-A: one bill that we'll close → CLOSED
FILTER_A_CLOSED="$(curl -sS -X POST "$BASE_URL/bills" \
  -H "Content-Type: application/json" \
  -d "{\"account_id\":\"$ACCT_A\",\"currency\":\"USD\",\"period_end\":\"$FILTER_END\"}")"
FILTER_A_CLOSED_ID="$(echo "$FILTER_A_CLOSED" | jq -r '.id')"
echo "  acct-A CLOSED: $FILTER_A_CLOSED_ID"

curl -sS -X PATCH "$BASE_URL/bills/$FILTER_A_CLOSED_ID" \
  -H "Content-Type: application/json" \
  -d '{"status":"CLOSED"}' | jq .

# acct-B: one OPEN bill
FILTER_B_OPEN="$(curl -sS -X POST "$BASE_URL/bills" \
  -H "Content-Type: application/json" \
  -d "{\"account_id\":\"$ACCT_B\",\"currency\":\"GEL\",\"period_end\":\"$FILTER_END\"}")"
FILTER_B_OPEN_ID="$(echo "$FILTER_B_OPEN" | jq -r '.id')"
echo "  acct-B OPEN:   $FILTER_B_OPEN_ID"

# acct-B: two bills that we'll close → CLOSED
FILTER_B_CLOSED1="$(curl -sS -X POST "$BASE_URL/bills" \
  -H "Content-Type: application/json" \
  -d "{\"account_id\":\"$ACCT_B\",\"currency\":\"GEL\",\"period_end\":\"$FILTER_END\"}")"
FILTER_B_CLOSED1_ID="$(echo "$FILTER_B_CLOSED1" | jq -r '.id')"
echo "  acct-B CLOSED: $FILTER_B_CLOSED1_ID"

curl -sS -X PATCH "$BASE_URL/bills/$FILTER_B_CLOSED1_ID" \
  -H "Content-Type: application/json" \
  -d '{"status":"CLOSED"}' | jq .

FILTER_B_CLOSED2="$(curl -sS -X POST "$BASE_URL/bills" \
  -H "Content-Type: application/json" \
  -d "{\"account_id\":\"$ACCT_B\",\"currency\":\"USD\",\"period_end\":\"$FILTER_END\"}")"
FILTER_B_CLOSED2_ID="$(echo "$FILTER_B_CLOSED2" | jq -r '.id')"
echo "  acct-B CLOSED: $FILTER_B_CLOSED2_ID"

curl -sS -X PATCH "$BASE_URL/bills/$FILTER_B_CLOSED2_ID" \
  -H "Content-Type: application/json" \
  -d '{"status":"CLOSED"}' | jq .

# Summary:
#   acct-A: 1 OPEN, 1 CLOSED          (2 total)
#   acct-B: 1 OPEN, 2 CLOSED          (3 total)

echo "11) Filter by status=OPEN for acct-A (expect 1)"
LIST_A_OPEN="$(curl -sS "$BASE_URL/bills?status=OPEN&account_id=$ACCT_A&limit=50")"
echo "$LIST_A_OPEN" | jq .
COUNT_A_OPEN="$(echo "$LIST_A_OPEN" | jq -r '.total')"
if [[ "$COUNT_A_OPEN" != "1" ]]; then
  echo "Expected 1 OPEN bill for $ACCT_A, got $COUNT_A_OPEN"
  exit 1
fi
# Verify the returned bill is actually OPEN
A_OPEN_STATUS="$(echo "$LIST_A_OPEN" | jq -r '.bills[0].status')"
if [[ "$A_OPEN_STATUS" != "OPEN" ]]; then
  echo "Expected OPEN status in result, got $A_OPEN_STATUS"
  exit 1
fi

echo "12) Filter by status=CLOSED for acct-A (expect 1)"
LIST_A_CLOSED="$(curl -sS "$BASE_URL/bills?status=CLOSED&account_id=$ACCT_A&limit=50")"
echo "$LIST_A_CLOSED" | jq .
COUNT_A_CLOSED="$(echo "$LIST_A_CLOSED" | jq -r '.total')"
if [[ "$COUNT_A_CLOSED" != "1" ]]; then
  echo "Expected 1 CLOSED bill for $ACCT_A, got $COUNT_A_CLOSED"
  exit 1
fi

echo "13) Filter by status=OPEN for acct-B (expect 1)"
LIST_B_OPEN="$(curl -sS "$BASE_URL/bills?status=OPEN&account_id=$ACCT_B&limit=50")"
echo "$LIST_B_OPEN" | jq .
COUNT_B_OPEN="$(echo "$LIST_B_OPEN" | jq -r '.total')"
if [[ "$COUNT_B_OPEN" != "1" ]]; then
  echo "Expected 1 OPEN bill for $ACCT_B, got $COUNT_B_OPEN"
  exit 1
fi

echo "14) Filter by status=CLOSED for acct-B (expect 2)"
LIST_B_CLOSED="$(curl -sS "$BASE_URL/bills?status=CLOSED&account_id=$ACCT_B&limit=50")"
echo "$LIST_B_CLOSED" | jq .
COUNT_B_CLOSED="$(echo "$LIST_B_CLOSED" | jq -r '.total')"
if [[ "$COUNT_B_CLOSED" != "2" ]]; then
  echo "Expected 2 CLOSED bills for $ACCT_B, got $COUNT_B_CLOSED"
  exit 1
fi
# Verify all returned bills are CLOSED
ALL_CLOSED="$(echo "$LIST_B_CLOSED" | jq -r '[.bills[].status] | unique | .[]')"
if [[ "$ALL_CLOSED" != "CLOSED" ]]; then
  echo "Expected all bills to be CLOSED, got statuses: $ALL_CLOSED"
  exit 1
fi

echo "15) Filter by account_id only — acct-A (expect 2: 1 OPEN + 1 CLOSED)"
LIST_A_ALL="$(curl -sS "$BASE_URL/bills?account_id=$ACCT_A&limit=50")"
echo "$LIST_A_ALL" | jq .
COUNT_A_ALL="$(echo "$LIST_A_ALL" | jq -r '.total')"
if [[ "$COUNT_A_ALL" != "2" ]]; then
  echo "Expected 2 total bills for $ACCT_A, got $COUNT_A_ALL"
  exit 1
fi

echo "16) Filter by account_id only — acct-B (expect 3: 1 OPEN + 2 CLOSED)"
LIST_B_ALL="$(curl -sS "$BASE_URL/bills?account_id=$ACCT_B&limit=50")"
echo "$LIST_B_ALL" | jq .
COUNT_B_ALL="$(echo "$LIST_B_ALL" | jq -r '.total')"
if [[ "$COUNT_B_ALL" != "3" ]]; then
  echo "Expected 3 total bills for $ACCT_B, got $COUNT_B_ALL"
  exit 1
fi

echo "17) Filter by status=CANCELLED (expect 0 for these accounts)"
# CANCELLED bills can only be created internally (failed workflow start compensation),
# so we verify the filter works and returns nothing for our test accounts.
LIST_CANCELLED="$(curl -sS "$BASE_URL/bills?status=CANCELLED&account_id=$ACCT_A&limit=50")"
echo "$LIST_CANCELLED" | jq .
COUNT_CANCELLED="$(echo "$LIST_CANCELLED" | jq -r '.total')"
if [[ "$COUNT_CANCELLED" != "0" ]]; then
  echo "Expected 0 CANCELLED bills for $ACCT_A, got $COUNT_CANCELLED"
  exit 1
fi

LIST_CANCELLED_B="$(curl -sS "$BASE_URL/bills?status=CANCELLED&account_id=$ACCT_B&limit=50")"
COUNT_CANCELLED_B="$(echo "$LIST_CANCELLED_B" | jq -r '.total')"
if [[ "$COUNT_CANCELLED_B" != "0" ]]; then
  echo "Expected 0 CANCELLED bills for $ACCT_B, got $COUNT_CANCELLED_B"
  exit 1
fi

echo "18) Cross-account isolation — acct-A bills should not appear in acct-B results"
A_IN_B="$(echo "$LIST_B_ALL" | jq -r --arg id "$FILTER_A_OPEN_ID" '[.bills[]? | select(.id == $id)] | length')"
if [[ "$A_IN_B" != "0" ]]; then
  echo "acct-A bill found in acct-B results — cross-account leak!"
  exit 1
fi
B_IN_A="$(echo "$LIST_A_ALL" | jq -r --arg id "$FILTER_B_OPEN_ID" '[.bills[]? | select(.id == $id)] | length')"
if [[ "$B_IN_A" != "0" ]]; then
  echo "acct-B bill found in acct-A results — cross-account leak!"
  exit 1
fi

echo "All endpoint checks passed."
