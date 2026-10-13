CREATE TABLE shop.invoice_settings (
    id boolean PRIMARY KEY DEFAULT true,
    shop_warning_minor bigint NOT NULL DEFAULT 100000000,
    external_planning_minor bigint NOT NULL DEFAULT 100000000,
    version bigint NOT NULL DEFAULT 1,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT invoice_settings_single_row CHECK (id),
    CONSTRAINT invoice_settings_warning_valid CHECK (shop_warning_minor >= 0),
    CONSTRAINT invoice_settings_planning_valid CHECK (external_planning_minor >= 0),
    CONSTRAINT invoice_settings_version_valid CHECK (version >= 1)
);

INSERT INTO shop.invoice_settings (id) VALUES (true);

-- Durable settings update dedupe: the result of each accepted update is kept
-- forever so an idempotency key replays its own outcome even after later
-- updates changed the live singleton.
CREATE TABLE shop.invoice_settings_requests (
    idempotency_key uuid PRIMARY KEY,
    payload_hash bytea NOT NULL,
    shop_warning_minor bigint NOT NULL,
    external_planning_minor bigint NOT NULL,
    version bigint NOT NULL,
    settings_updated_at timestamptz NOT NULL,
    actor_user_id uuid REFERENCES shop.users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT invoice_settings_requests_hash_valid CHECK (octet_length(payload_hash) = 32),
    CONSTRAINT invoice_settings_requests_warning_valid CHECK (shop_warning_minor >= 0),
    CONSTRAINT invoice_settings_requests_planning_valid CHECK (external_planning_minor >= 0),
    CONSTRAINT invoice_settings_requests_version_valid CHECK (version >= 1)
);

CREATE TRIGGER invoice_settings_requests_immutable
BEFORE UPDATE OR DELETE ON shop.invoice_settings_requests
FOR EACH ROW EXECUTE FUNCTION shop.reject_immutable_row_change();

CREATE TABLE shop.invoice_counters (
    year integer PRIMARY KEY,
    next_number bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT invoice_counters_year_valid CHECK (year BETWEEN 2000 AND 9999),
    CONSTRAINT invoice_counters_next_valid CHECK (next_number >= 1)
);

CREATE TABLE shop.invoices (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    number text NOT NULL,
    kind text NOT NULL,
    sequence_year integer NOT NULL,
    sequence_number bigint NOT NULL,
    order_id uuid REFERENCES shop.orders(id) ON DELETE RESTRICT,
    order_number bigint,
    original_invoice_id uuid REFERENCES shop.invoices(id) ON DELETE RESTRICT,
    refund_reference text,
    refunded_on date,
    currency text NOT NULL DEFAULT 'CZK',
    total_minor bigint NOT NULL,
    items_minor bigint NOT NULL,
    shipping_minor bigint NOT NULL,
    discount_minor bigint NOT NULL,
    supply_date date,
    issued_date date NOT NULL,
    issued_at timestamptz NOT NULL,
    paid boolean NOT NULL DEFAULT false,
    reason text,
    locale text NOT NULL DEFAULT 'cs-CZ',
    buyer_country text NOT NULL DEFAULT '',
    snapshot jsonb NOT NULL,
    actor_user_id uuid REFERENCES shop.users(id) ON DELETE SET NULL,
    idempotency_key uuid,
    idempotency_payload_hash bytea,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT invoices_kind_valid CHECK (kind IN ('invoice', 'credit_note')),
    CONSTRAINT invoices_currency_valid CHECK (currency = 'CZK'),
    CONSTRAINT invoices_number_unique UNIQUE (number),
    CONSTRAINT invoices_sequence_unique UNIQUE (sequence_year, sequence_number),
    CONSTRAINT invoices_year_valid CHECK (sequence_year BETWEEN 2000 AND 9999),
    CONSTRAINT invoices_snapshot_object CHECK (jsonb_typeof(snapshot) = 'object'),
    CONSTRAINT invoices_payload_hash_valid CHECK (
        idempotency_payload_hash IS NULL OR octet_length(idempotency_payload_hash) = 32
    ),
    CONSTRAINT invoices_reconcile CHECK (
        items_minor + shipping_minor - discount_minor = total_minor
    ),
    CONSTRAINT invoices_invoice_sign CHECK (kind <> 'invoice' OR total_minor >= 0),
    CONSTRAINT invoices_credit_sign CHECK (kind <> 'credit_note' OR total_minor <= 0),
    CONSTRAINT invoices_credit_link CHECK (
        (
            kind = 'invoice'
            AND original_invoice_id IS NULL
            AND refund_reference IS NULL
            AND refunded_on IS NULL
        )
        OR (
            kind = 'credit_note'
            AND original_invoice_id IS NOT NULL
            AND refund_reference IS NOT NULL
            AND refunded_on IS NOT NULL
        )
    )
);

CREATE UNIQUE INDEX invoices_order_invoice_unique
    ON shop.invoices (order_id) WHERE kind = 'invoice';
CREATE UNIQUE INDEX invoices_idempotency_unique
    ON shop.invoices (idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX invoices_year_idx ON shop.invoices (sequence_year, sequence_number);
CREATE INDEX invoices_order_idx ON shop.invoices (order_id);
CREATE INDEX invoices_original_idx
    ON shop.invoices (original_invoice_id) WHERE original_invoice_id IS NOT NULL;

CREATE TRIGGER invoices_immutable
BEFORE UPDATE OR DELETE ON shop.invoices
FOR EACH ROW EXECUTE FUNCTION shop.reject_immutable_row_change();

CREATE TABLE shop.external_turnover_entries (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    occurred_on date NOT NULL,
    amount_minor bigint NOT NULL,
    country_code text NOT NULL DEFAULT '',
    classification text NOT NULL,
    note text NOT NULL DEFAULT '',
    reversal_of uuid REFERENCES shop.external_turnover_entries(id) ON DELETE RESTRICT,
    reason text,
    actor_user_id uuid REFERENCES shop.users(id) ON DELETE SET NULL,
    idempotency_key uuid,
    idempotency_payload_hash bytea,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT external_turnover_classification_valid CHECK (
        classification IN ('pending', 'domestic', 'eu_tbe', 'excluded')
    ),
    CONSTRAINT external_turnover_amount_valid CHECK (amount_minor <> 0),
    CONSTRAINT external_turnover_country_valid CHECK (
        country_code = '' OR country_code ~ '^[A-Z]{2}$'
    ),
    CONSTRAINT external_turnover_reversal_valid CHECK (
        (reversal_of IS NULL AND amount_minor > 0 AND reason IS NULL)
        OR (reversal_of IS NOT NULL AND amount_minor < 0 AND reason IS NOT NULL)
    ),
    CONSTRAINT external_turnover_payload_hash_valid CHECK (
        idempotency_payload_hash IS NULL OR octet_length(idempotency_payload_hash) = 32
    )
);

CREATE UNIQUE INDEX external_turnover_idempotency_unique
    ON shop.external_turnover_entries (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX external_turnover_reversal_unique
    ON shop.external_turnover_entries (reversal_of)
    WHERE reversal_of IS NOT NULL;
CREATE INDEX external_turnover_year_idx ON shop.external_turnover_entries (occurred_on);

CREATE TRIGGER external_turnover_immutable
BEFORE UPDATE OR DELETE ON shop.external_turnover_entries
FOR EACH ROW EXECUTE FUNCTION shop.reject_immutable_row_change();

CREATE TABLE shop.turnover_alerts (
    year integer NOT NULL,
    metric text NOT NULL,
    stage smallint NOT NULL,
    value_minor bigint NOT NULL,
    threshold_minor bigint NOT NULL,
    triggered_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (year, metric, stage),
    CONSTRAINT turnover_alerts_year_valid CHECK (year BETWEEN 2000 AND 9999),
    CONSTRAINT turnover_alerts_metric_valid CHECK (
        metric IN ('eu_tbe', 'own_warning', 'legal_czk')
    ),
    CONSTRAINT turnover_alerts_stage_valid CHECK (stage IN (80, 90, 100)),
    CONSTRAINT turnover_alerts_value_valid CHECK (value_minor >= 0),
    CONSTRAINT turnover_alerts_threshold_valid CHECK (threshold_minor > 0)
);

CREATE TRIGGER turnover_alerts_immutable
BEFORE UPDATE OR DELETE ON shop.turnover_alerts
FOR EACH ROW EXECUTE FUNCTION shop.reject_immutable_row_change();
