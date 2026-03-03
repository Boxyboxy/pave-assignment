CREATE TABLE bills (
  id UUID PRIMARY KEY,
  account_id TEXT NOT NULL,
  currency TEXT NOT NULL CHECK (currency IN ('USD', 'GEL')),
  status TEXT NOT NULL CHECK (status IN ('OPEN', 'CLOSED', 'CANCELLED')),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  period_start TIMESTAMPTZ NOT NULL,
  period_end TIMESTAMPTZ NOT NULL,
  closed_at TIMESTAMPTZ NULL,
  total_minor BIGINT NOT NULL DEFAULT 0,
  workflow_id TEXT NOT NULL,
  workflow_run_id TEXT NULL
);

CREATE INDEX bills_status_idx ON bills(status);
CREATE INDEX bills_account_id_status_idx ON bills(account_id, status);
CREATE INDEX bills_period_end_idx ON bills(period_end);
CREATE INDEX bills_workflow_run_id_null_idx ON bills(created_at) WHERE workflow_run_id IS NULL;

CREATE TABLE line_items (
  id UUID PRIMARY KEY,
  bill_id UUID NOT NULL REFERENCES bills(id),
  idempotency_key TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  description TEXT NOT NULL,
  amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE (bill_id, idempotency_key)
);

CREATE INDEX line_items_bill_id_created_at_idx ON line_items(bill_id, created_at);
