package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/m1chlcz/invoice-ledger"
)

const (
	dateLayout = "2006-01-02"

	minSequenceYear = 2000
	maxSequenceYear = 9999
)

// Store is a PostgreSQL-backed ledger store. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

var (
	_ invoiceledger.Store          = (*Store)(nil)
	_ invoiceledger.CreditInserter = (*Store)(nil)
	_ invoiceledger.TurnoverStore  = (*Store)(nil)
	_ invoiceledger.SettingsStore  = (*Store)(nil)
	_ invoiceledger.DocumentLister = (*Store)(nil)
	_ invoiceledger.Source         = (*Store)(nil)
)

// New returns a Store backed by an existing pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Open parses databaseURL, opens a pool and verifies the connection. The
// caller must call Close when the Store is no longer needed.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("invoiceledger/postgres: parse database URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("invoiceledger/postgres: connect: %w", err)
	}
	if pingErr := pool.Ping(ctx); pingErr != nil {
		pool.Close()
		return nil, fmt.Errorf("invoiceledger/postgres: connect: %w", pingErr)
	}
	return New(pool), nil
}

// Close releases the underlying connection pool.
func (store *Store) Close() {
	if store.pool != nil {
		store.pool.Close()
	}
}

const documentSelect = `
	SELECT i.id, i.number, i.kind, i.sequence_year, i.sequence_number,
	       i.order_id, i.order_number,
	       i.original_invoice_id, orig.number,
	       i.refund_reference, i.refunded_on, i.currency, i.total_minor,
	       i.items_minor, i.shipping_minor, i.discount_minor,
	       i.supply_date, i.issued_date, i.issued_at, i.paid, i.reason,
	       i.locale, i.buyer_country, i.snapshot, i.created_at,
	       i.idempotency_key, i.idempotency_payload_hash
	FROM shop.invoices i
	LEFT JOIN shop.invoices orig ON orig.id = i.original_invoice_id`

const allocateNumberSQL = `
	INSERT INTO shop.invoice_counters (year, next_number)
	VALUES ($1, 2)
	ON CONFLICT (year) DO UPDATE
	SET next_number = shop.invoice_counters.next_number + 1,
	    updated_at = now()
	RETURNING next_number - 1`

const insertDocumentSQL = `
	INSERT INTO shop.invoices (
		id, number, kind, sequence_year, sequence_number, order_id, order_number,
		original_invoice_id, refund_reference, refunded_on, currency, total_minor,
		items_minor, shipping_minor, discount_minor, supply_date, issued_date,
		issued_at, paid, reason, locale, buyer_country, snapshot, created_at,
		idempotency_key, idempotency_payload_hash
	) VALUES (
		$1, $2, $3, $4, $5, $6, $7,
		$8, $9, $10, $11, $12,
		$13, $14, $15, $16, $17,
		$18, $19, $20, $21, $22, $23, $24,
		$25, $26
	)`

type rowExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// AllocateNumber implements invoiceledger.Store. The counter row is the
// shared series for both kinds; a single upsert allocates without a gap-free
// transaction.
func (store *Store) AllocateNumber(
	ctx context.Context,
	year int,
	kind invoiceledger.Kind,
) (invoiceledger.Sequence, error) {
	if year < minSequenceYear || year > maxSequenceYear {
		return invoiceledger.Sequence{}, invoiceledger.ErrUnprocessable
	}
	var number int64
	err := store.pool.QueryRow(ctx, allocateNumberSQL, year).Scan(&number)
	if err != nil {
		return invoiceledger.Sequence{}, translate("allocate invoice number", err)
	}
	return invoiceledger.Sequence{Year: year, Kind: kind, Number: number}, nil
}

// Insert implements invoiceledger.Store. The document must carry the fields
// the shop schema marks as required: a valid issued date and amount signs and
// totals that satisfy the schema constraints. Insert returns ErrConflict for
// a duplicate ID, number, sequence pair, idempotency key, or a second invoice
// for one order.
func (store *Store) Insert(
	ctx context.Context,
	document invoiceledger.Document,
) (invoiceledger.Document, error) {
	stored, err := prepareDocument(document)
	if err != nil {
		return invoiceledger.Document{}, err
	}
	if err := store.insertDocument(ctx, store.pool, stored, stored.OriginalID); err != nil {
		return invoiceledger.Document{}, err
	}
	return stored, nil
}

// InsertCredit implements invoiceledger.CreditInserter. The original invoice
// row is locked, the credited total is read, and the insert commits in one
// transaction, so two processes with the same store cannot over-credit.
func (store *Store) InsertCredit(
	ctx context.Context,
	document invoiceledger.Document,
	originalID uuid.UUID,
	maxTotalMinor int64,
) (invoiceledger.Document, error) {
	if document.Kind != invoiceledger.KindCreditNote || document.TotalMinor > 0 || maxTotalMinor < 0 {
		return invoiceledger.Document{}, invoiceledger.ErrUnprocessable
	}
	stored, err := prepareDocument(document)
	if err != nil {
		return invoiceledger.Document{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return invoiceledger.Document{}, translate("begin credit transaction", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var locked uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM shop.invoices
		WHERE id = $1 AND kind = 'invoice'
		FOR UPDATE`, originalID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return invoiceledger.Document{}, invoiceledger.ErrConflict
	}
	if err != nil {
		return invoiceledger.Document{}, translate("lock original invoice", err)
	}
	var credited int64
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(total_minor), 0)
		FROM shop.invoices
		WHERE original_invoice_id = $1 AND kind = 'credit_note'`, originalID).Scan(&credited)
	if err != nil {
		return invoiceledger.Document{}, translate("read credited total", err)
	}
	projected, ok := addInt64(credited, stored.TotalMinor)
	if !ok || projected < -maxTotalMinor {
		return invoiceledger.Document{}, invoiceledger.ErrConflict
	}
	stored.OriginalID = &originalID
	if insertErr := store.insertDocument(ctx, tx, stored, &originalID); insertErr != nil {
		return invoiceledger.Document{}, insertErr
	}
	if err := tx.Commit(ctx); err != nil {
		return invoiceledger.Document{}, translate("commit credit transaction", err)
	}
	return stored, nil
}

// ByID implements invoiceledger.Store.
func (store *Store) ByID(ctx context.Context, id uuid.UUID) (invoiceledger.Document, error) {
	document, err := scanDocument(store.pool.QueryRow(ctx, documentSelect+` WHERE i.id = $1`, id))
	if err != nil {
		return invoiceledger.Document{}, translate("read document", err)
	}
	return document, nil
}

// ByOrder implements invoiceledger.Store.
func (store *Store) ByOrder(ctx context.Context, orderID uuid.UUID) ([]invoiceledger.Document, error) {
	rows, err := store.pool.Query(ctx, documentSelect+`
		WHERE i.order_id = $1
		   OR i.original_invoice_id IN (
			SELECT id FROM shop.invoices WHERE order_id = $1
		   )
		ORDER BY i.sequence_year, i.sequence_number`, orderID)
	if err != nil {
		return nil, translate("read order documents", err)
	}
	return scanDocuments(rows)
}

// ByIdempotencyKey implements invoiceledger.Store.
func (store *Store) ByIdempotencyKey(ctx context.Context, key uuid.UUID) (invoiceledger.Document, error) {
	if key == uuid.Nil {
		return invoiceledger.Document{}, invoiceledger.ErrNotFound
	}
	document, err := scanDocument(
		store.pool.QueryRow(ctx, documentSelect+` WHERE i.idempotency_key = $1`, key),
	)
	if err != nil {
		return invoiceledger.Document{}, translate("read document by idempotency key", err)
	}
	return document, nil
}

// CreditedTotal implements invoiceledger.Store.
func (store *Store) CreditedTotal(ctx context.Context, originalID uuid.UUID) (int64, error) {
	var total int64
	err := store.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(total_minor), 0)
		FROM shop.invoices
		WHERE original_invoice_id = $1 AND kind = 'credit_note'`, originalID).Scan(&total)
	if err != nil {
		return 0, translate("read credited total", err)
	}
	return total, nil
}

// ListDocuments implements invoiceledger.DocumentLister.
func (store *Store) ListDocuments(ctx context.Context) ([]invoiceledger.Document, error) {
	rows, err := store.pool.Query(ctx, documentSelect+`
		ORDER BY i.sequence_year, i.sequence_number`)
	if err != nil {
		return nil, translate("list documents", err)
	}
	return scanDocuments(rows)
}

func scanDocuments(rows pgx.Rows) ([]invoiceledger.Document, error) {
	defer rows.Close()
	documents := make([]invoiceledger.Document, 0)
	for rows.Next() {
		document, err := scanDocument(rows)
		if err != nil {
			return nil, translate("read document", err)
		}
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		return nil, translate("read documents", err)
	}
	return documents, nil
}

func scanDocument(row pgx.Row) (invoiceledger.Document, error) {
	var (
		document        invoiceledger.Document
		kind            string
		orderNumber     *int64
		originalNumber  *string
		refundReference *string
		refundedOn      *time.Time
		supplyDate      *time.Time
		issuedDate      time.Time
		paid            bool
		reason          *string
		snapshot        []byte
		createdAt       time.Time
		key             *uuid.UUID
		hash            []byte
	)
	err := row.Scan(
		&document.ID, &document.Number, &kind, &document.SequenceYear, &document.SequenceNumber,
		&document.OrderID, &orderNumber,
		&document.OriginalID, &originalNumber,
		&refundReference, &refundedOn, &document.Currency, &document.TotalMinor,
		&document.ItemsMinor, &document.ShippingMinor, &document.DiscountMinor,
		&supplyDate, &issuedDate, &document.IssuedAt, &paid, &reason,
		&document.Locale, &document.BuyerCountry, &snapshot, &createdAt,
		&key, &hash,
	)
	if err != nil {
		return invoiceledger.Document{}, err
	}
	document.Kind = invoiceledger.Kind(kind)
	if orderNumber != nil {
		document.OrderNumber = strconv.FormatInt(*orderNumber, 10)
	}
	if originalNumber != nil {
		document.OriginalNumber = *originalNumber
	}
	if refundReference != nil {
		document.RefundReference = *refundReference
	}
	if refundedOn != nil {
		document.RefundedOn = refundedOn.Format(dateLayout)
	}
	if supplyDate != nil {
		document.SupplyDate = supplyDate.Format(dateLayout)
	}
	document.IssuedDate = issuedDate.Format(dateLayout)
	document.IssuedAt = document.IssuedAt.UTC()
	document.Paid = paid
	if reason != nil {
		document.Reason = *reason
	}
	decoded, err := decodeSnapshot(snapshot)
	if err != nil {
		return invoiceledger.Document{}, err
	}
	document.Snapshot = decoded
	document.CreatedAt = createdAt.UTC()
	document.IdempotencyKey = key
	document.IdempotencyPayloadHash = hash
	return document, nil
}

func (store *Store) insertDocument(
	ctx context.Context,
	executor rowExecutor,
	document invoiceledger.Document,
	originalID *uuid.UUID,
) error {
	orderNumber, err := optionalInt64(document.OrderNumber)
	if err != nil {
		return err
	}
	refundedOn, err := optionalDate(document.RefundedOn)
	if err != nil {
		return err
	}
	supplyDate, err := optionalDate(document.SupplyDate)
	if err != nil {
		return err
	}
	issuedDate, err := requiredDate(document.IssuedDate)
	if err != nil {
		return err
	}
	snapshot, err := encodeSnapshot(document.Snapshot)
	if err != nil {
		return err
	}
	_, err = executor.Exec(ctx, insertDocumentSQL,
		document.ID,
		document.Number,
		string(document.Kind),
		document.SequenceYear,
		document.SequenceNumber,
		document.OrderID,
		orderNumber,
		originalID,
		optionalString(document.RefundReference),
		refundedOn,
		document.Currency,
		document.TotalMinor,
		document.ItemsMinor,
		document.ShippingMinor,
		document.DiscountMinor,
		supplyDate,
		issuedDate,
		document.IssuedAt,
		document.Paid,
		optionalString(document.Reason),
		document.Locale,
		document.BuyerCountry,
		snapshot,
		document.CreatedAt,
		document.IdempotencyKey,
		document.IdempotencyPayloadHash,
	)
	if err != nil {
		return translate("insert document", err)
	}
	return nil
}

func prepareDocument(document invoiceledger.Document) (invoiceledger.Document, error) {
	stored := document
	if stored.ID == uuid.Nil {
		stored.ID = uuid.New()
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	stored.CreatedAt = normalizedTime(stored.CreatedAt, now)
	stored.IssuedAt = normalizedTime(stored.IssuedAt, now)
	if stored.IssuedDate == "" || !validDate(stored.IssuedDate) {
		return invoiceledger.Document{}, invoiceledger.ErrUnprocessable
	}
	return stored, nil
}

func normalizedTime(value, fallback time.Time) time.Time {
	if value.IsZero() {
		return fallback
	}
	return value.UTC().Truncate(time.Microsecond)
}

func validDate(value string) bool {
	parsed, err := time.Parse(dateLayout, value)
	return err == nil && parsed.Format(dateLayout) == value
}

func optionalDate(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(dateLayout, value)
	if err != nil || parsed.Format(dateLayout) != value {
		return nil, invoiceledger.ErrUnprocessable
	}
	return &parsed, nil
}

func requiredDate(value string) (time.Time, error) {
	parsed, err := time.Parse(dateLayout, value)
	if err != nil || parsed.Format(dateLayout) != value {
		return time.Time{}, invoiceledger.ErrUnprocessable
	}
	return parsed, nil
}

func optionalInt64(value string) (*int64, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil, invoiceledger.ErrUnprocessable
	}
	return &parsed, nil
}

func optionalString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func addInt64(current, delta int64) (int64, bool) {
	if delta > 0 && current > math.MaxInt64-delta {
		return 0, false
	}
	if delta < 0 && current < math.MinInt64-delta {
		return 0, false
	}
	return current + delta, true
}
