CREATE TABLE skus (
    id bigserial PRIMARY KEY,
    code text NOT NULL UNIQUE,
    name text NOT NULL,
    image_url text NOT NULL
);

CREATE TABLE parties (
    id bigserial PRIMARY KEY,
    name text NOT NULL,
    phone text NOT NULL
);

CREATE TABLE jobwork_issues (
    id bigserial PRIMARY KEY,
    party_id bigint NOT NULL REFERENCES parties(id),
    sku_id bigint NOT NULL REFERENCES skus(id),
    qty bigint NOT NULL CHECK (qty > 0),
    issued_on date NOT NULL
);

CREATE TABLE jobwork_receipts (
    id bigserial PRIMARY KEY,
    issue_id bigint NOT NULL REFERENCES jobwork_issues(id),
    qty bigint NOT NULL CHECK (qty > 0),
    received_on date NOT NULL,
    idempotency_key text NOT NULL UNIQUE,
    request_fingerprint text NOT NULL,
    response_json jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (length(idempotency_key) BETWEEN 1 AND 200)
);
CREATE INDEX jobwork_receipts_issue_id_idx ON jobwork_receipts(issue_id);

CREATE TABLE vouchers (
    id bigserial PRIMARY KEY,
    type text NOT NULL,
    source_id bigint NOT NULL UNIQUE REFERENCES jobwork_receipts(id),
    amount_paise bigint NOT NULL CHECK (amount_paise > 0),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'pushed', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error text,
    client_ref text NOT NULL UNIQUE,
    external_ref text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_token text,
    lease_until timestamptz
);
CREATE INDEX vouchers_due_idx ON vouchers(next_attempt_at, id) WHERE status = 'pending';
CREATE INDEX vouchers_status_idx ON vouchers(status, id);

CREATE TABLE mock_accounting_vouchers (
    client_ref text PRIMARY KEY,
    external_ref text NOT NULL UNIQUE,
    amount_paise bigint NOT NULL CHECK (amount_paise > 0),
    type text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
