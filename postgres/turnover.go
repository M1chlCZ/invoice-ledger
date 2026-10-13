package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/m1chlcz/invoice-ledger"
)

const turnoverSelect = `
	SELECT id, occurred_on, amount_minor, country_code, classification, note,
	       reversal_of, reason, created_at, idempotency_key, idempotency_payload_hash
	FROM shop.external_turnover_entries`

const insertTurnoverSQL = `
	INSERT INTO shop.external_turnover_entries (
		id, occurred_on, amount_minor, country_code, classification, note,
		created_at, idempotency_key, idempotency_payload_hash
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

const insertTurnoverReversalSQL = `
	INSERT INTO shop.external_turnover_entries (
		id, occurred_on, amount_minor, country_code, classification, note,
		reversal_of, reason, created_at, idempotency_key, idempotency_payload_hash
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

// InsertTurnover implements invoiceledger.TurnoverStore.
func (store *Store) InsertTurnover(
	ctx context.Context,
	entry invoiceledger.TurnoverEntry,
) (invoiceledger.TurnoverEntry, error) {
	if entry.ReversalOf != nil || entry.AmountMinor <= 0 || strings.TrimSpace(entry.Reason) != "" {
		return invoiceledger.TurnoverEntry{}, invoiceledger.ErrUnprocessable
	}
	occurredOn, err := optionalDate(entry.OccurredOn)
	if err != nil || occurredOn == nil {
		return invoiceledger.TurnoverEntry{}, invoiceledger.ErrUnprocessable
	}
	stored := prepareTurnover(entry)
	_, err = store.pool.Exec(ctx, insertTurnoverSQL,
		stored.ID, occurredOn, stored.AmountMinor, stored.CountryCode,
		string(stored.Classification), stored.Note, stored.CreatedAt,
		stored.IdempotencyKey, stored.IdempotencyPayloadHash,
	)
	if err != nil {
		return invoiceledger.TurnoverEntry{}, translate("insert turnover entry", err)
	}
	return stored, nil
}

// InsertTurnoverReversal implements invoiceledger.TurnoverStore. The target
// row is locked and the signed amount and the classification fields are
// derived from it inside one transaction.
func (store *Store) InsertTurnoverReversal(
	ctx context.Context,
	entry invoiceledger.TurnoverEntry,
) (invoiceledger.TurnoverEntry, error) {
	if entry.ReversalOf == nil || *entry.ReversalOf == uuid.Nil ||
		strings.TrimSpace(entry.Reason) == "" {
		return invoiceledger.TurnoverEntry{}, invoiceledger.ErrUnprocessable
	}
	stored := prepareTurnover(entry)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return invoiceledger.TurnoverEntry{}, translate("begin turnover reversal transaction", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var (
		occurredOn     time.Time
		classification string
		reversalOf     *uuid.UUID
	)
	err = tx.QueryRow(ctx, `
		SELECT occurred_on, amount_minor, country_code, classification, note, reversal_of
		FROM shop.external_turnover_entries
		WHERE id = $1
		FOR UPDATE`, *stored.ReversalOf).Scan(
		&occurredOn, &stored.AmountMinor, &stored.CountryCode,
		&classification, &stored.Note, &reversalOf,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return invoiceledger.TurnoverEntry{}, invoiceledger.ErrNotFound
	}
	if err != nil {
		return invoiceledger.TurnoverEntry{}, translate("lock turnover target", err)
	}
	if reversalOf != nil || stored.AmountMinor <= 0 {
		return invoiceledger.TurnoverEntry{}, invoiceledger.ErrConflict
	}
	var reversed bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM shop.external_turnover_entries WHERE reversal_of = $1
		)`, *stored.ReversalOf).Scan(&reversed)
	if err != nil {
		return invoiceledger.TurnoverEntry{}, translate("read turnover reversal", err)
	}
	if reversed {
		return invoiceledger.TurnoverEntry{}, invoiceledger.ErrConflict
	}
	stored.OccurredOn = occurredOn.Format(dateLayout)
	stored.AmountMinor = -stored.AmountMinor
	stored.Classification = invoiceledger.TurnoverClassification(classification)
	_, err = tx.Exec(ctx, insertTurnoverReversalSQL,
		stored.ID, occurredOn, stored.AmountMinor, stored.CountryCode,
		string(stored.Classification), stored.Note, *stored.ReversalOf,
		stored.Reason, stored.CreatedAt, stored.IdempotencyKey,
		stored.IdempotencyPayloadHash,
	)
	if err != nil {
		return invoiceledger.TurnoverEntry{}, translate("insert turnover reversal", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return invoiceledger.TurnoverEntry{}, translate("commit turnover reversal transaction", err)
	}
	return stored, nil
}

// TurnoverByID implements invoiceledger.TurnoverStore.
func (store *Store) TurnoverByID(ctx context.Context, id uuid.UUID) (invoiceledger.TurnoverEntry, error) {
	entry, err := scanTurnover(store.pool.QueryRow(ctx, turnoverSelect+` WHERE id = $1`, id))
	if err != nil {
		return invoiceledger.TurnoverEntry{}, translate("read turnover entry", err)
	}
	return entry, nil
}

// TurnoverByIdempotencyKey implements invoiceledger.TurnoverStore.
func (store *Store) TurnoverByIdempotencyKey(
	ctx context.Context,
	key uuid.UUID,
) (invoiceledger.TurnoverEntry, error) {
	if key == uuid.Nil {
		return invoiceledger.TurnoverEntry{}, invoiceledger.ErrNotFound
	}
	entry, err := scanTurnover(
		store.pool.QueryRow(ctx, turnoverSelect+` WHERE idempotency_key = $1`, key),
	)
	if err != nil {
		return invoiceledger.TurnoverEntry{}, translate("read turnover entry by idempotency key", err)
	}
	return entry, nil
}

// TurnoverByYear implements invoiceledger.TurnoverStore.
func (store *Store) TurnoverByYear(ctx context.Context, year int) ([]invoiceledger.TurnoverEntry, error) {
	if year < 0 || year > maxSequenceYear {
		return nil, invoiceledger.ErrUnprocessable
	}
	query := turnoverSelect
	var args []any
	if year > 0 {
		query += ` WHERE occurred_on >= $1 AND occurred_on < $2`
		start := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
		args = append(args, start, start.AddDate(1, 0, 0))
	}
	query += ` ORDER BY created_at DESC, id`
	rows, err := store.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, translate("list turnover entries", err)
	}
	defer rows.Close()
	entries := make([]invoiceledger.TurnoverEntry, 0)
	for rows.Next() {
		entry, scanErr := scanTurnover(rows)
		if scanErr != nil {
			return nil, translate("read turnover entry", scanErr)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, translate("read turnover entries", err)
	}
	return entries, nil
}

func scanTurnover(row pgx.Row) (invoiceledger.TurnoverEntry, error) {
	var (
		entry          invoiceledger.TurnoverEntry
		occurredOn     time.Time
		classification string
		reason         *string
		createdAt      time.Time
		key            *uuid.UUID
		hash           []byte
	)
	err := row.Scan(
		&entry.ID, &occurredOn, &entry.AmountMinor, &entry.CountryCode,
		&classification, &entry.Note, &entry.ReversalOf, &reason, &createdAt,
		&key, &hash,
	)
	if err != nil {
		return invoiceledger.TurnoverEntry{}, err
	}
	entry.OccurredOn = occurredOn.Format(dateLayout)
	entry.Classification = invoiceledger.TurnoverClassification(classification)
	if reason != nil {
		entry.Reason = *reason
	}
	entry.CreatedAt = createdAt.UTC()
	entry.IdempotencyKey = key
	entry.IdempotencyPayloadHash = hash
	return entry, nil
}

func prepareTurnover(entry invoiceledger.TurnoverEntry) invoiceledger.TurnoverEntry {
	stored := entry
	if stored.ID == uuid.Nil {
		stored.ID = uuid.New()
	}
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	} else {
		stored.CreatedAt = stored.CreatedAt.UTC().Truncate(time.Microsecond)
	}
	return stored
}
