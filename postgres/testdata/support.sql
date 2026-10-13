-- Minimal stand-ins for objects owned by earlier shop migrations. The
-- integration test applies these before 000106_invoice_ledger.sql so its
-- foreign keys and triggers resolve in the disposable test database.

CREATE TABLE shop.users (
    id uuid PRIMARY KEY
);

CREATE FUNCTION shop.reject_immutable_row_change()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
BEGIN
    RAISE EXCEPTION 'immutable row in %.% cannot be changed', TG_TABLE_SCHEMA, TG_TABLE_NAME
        USING ERRCODE = '55000';
END
$function$;

CREATE TABLE shop.orders (
    id uuid PRIMARY KEY,
    order_number bigint NOT NULL UNIQUE,
    contact_email text NOT NULL,
    email_locale text NOT NULL DEFAULT 'cs-CZ',
    currency text NOT NULL DEFAULT 'CZK',
    billing_address jsonb NOT NULL,
    shipping_address jsonb NOT NULL,
    items_total_minor bigint NOT NULL,
    shipping_total_minor bigint NOT NULL,
    discount_total_minor bigint NOT NULL DEFAULT 0,
    grand_total_minor bigint NOT NULL,
    order_status text NOT NULL,
    payment_status text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE shop.order_items (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id uuid NOT NULL REFERENCES shop.orders(id) ON DELETE CASCADE,
    name text NOT NULL,
    customization jsonb NOT NULL DEFAULT '{}'::jsonb,
    quantity integer NOT NULL,
    unit_price_minor bigint NOT NULL,
    line_total_minor bigint NOT NULL,
    tax_rate_basis_points integer NOT NULL DEFAULT 0,
    tax_minor bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE shop.order_shipping (
    order_id uuid PRIMARY KEY REFERENCES shop.orders(id) ON DELETE CASCADE,
    method_name text,
    tax_rate_basis_points integer,
    tax_minor bigint
);
