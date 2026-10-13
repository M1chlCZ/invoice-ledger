package postgres

import (
	"bytes"
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/m1chlcz/invoice-ledger"
)

const settingsSelect = `
	SELECT shop_warning_minor, external_planning_minor, version, updated_at
	FROM shop.invoice_settings
	WHERE id`

const lockSettingsSQL = `
	SELECT shop_warning_minor, external_planning_minor, version, updated_at
	FROM shop.invoice_settings
	WHERE id
	FOR UPDATE`

const updateSettingsSQL = `
	UPDATE shop.invoice_settings
	SET shop_warning_minor = $1,
	    external_planning_minor = $2,
	    version = version + 1,
	    updated_at = now()
	WHERE id
	RETURNING shop_warning_minor, external_planning_minor, version, updated_at`

const insertSettingsRequestSQL = `
	INSERT INTO shop.invoice_settings_requests (
		idempotency_key, payload_hash, shop_warning_minor,
		external_planning_minor, version, settings_updated_at
	) VALUES ($1, $2, $3, $4, $5, $6)`

type settingsRequestRow struct {
	payloadHash []byte
	settings    invoiceledger.Settings
}

// GetSettings implements invoiceledger.SettingsStore.
func (store *Store) GetSettings(ctx context.Context) (invoiceledger.Settings, error) {
	var settings invoiceledger.Settings
	err := store.pool.QueryRow(ctx, settingsSelect).Scan(
		&settings.ShopWarningMinor, &settings.ExternalPlanningMinor,
		&settings.Version, &settings.UpdatedAt,
	)
	if err != nil {
		return invoiceledger.Settings{}, translate("read invoice settings", err)
	}
	settings.UpdatedAt = settings.UpdatedAt.UTC()
	return settings, nil
}

// UpdateSettings implements invoiceledger.SettingsStore. The singleton row is
// locked before the version check, and a concurrent request with the same key
// is rechecked after the lock before the version conflict is reported.
func (store *Store) UpdateSettings(
	ctx context.Context,
	update invoiceledger.SettingsUpdate,
	payloadHash []byte,
) (invoiceledger.Settings, bool, error) {
	if update.ShopWarningMinor < 0 || update.ExternalPlanningMinor < 0 || update.Version < 1 {
		return invoiceledger.Settings{}, false, invoiceledger.ErrUnprocessable
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return invoiceledger.Settings{}, false, translate("begin settings transaction", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	replay, found, err := readSettingsRequestTx(ctx, tx, update.IdempotencyKey)
	if err != nil {
		return invoiceledger.Settings{}, false, err
	}
	if found {
		if !bytes.Equal(replay.payloadHash, payloadHash) {
			return invoiceledger.Settings{}, false, invoiceledger.ErrIdempotencyConflict
		}
		return replay.settings, true, nil
	}
	var current invoiceledger.Settings
	err = tx.QueryRow(ctx, lockSettingsSQL).Scan(
		&current.ShopWarningMinor, &current.ExternalPlanningMinor,
		&current.Version, &current.UpdatedAt,
	)
	if err != nil {
		return invoiceledger.Settings{}, false, translate("lock invoice settings", err)
	}
	if current.Version != update.Version {
		recheck, recheckFound, recheckErr := readSettingsRequestTx(ctx, tx, update.IdempotencyKey)
		if recheckErr != nil {
			return invoiceledger.Settings{}, false, recheckErr
		}
		if recheckFound {
			if !bytes.Equal(recheck.payloadHash, payloadHash) {
				return invoiceledger.Settings{}, false, invoiceledger.ErrIdempotencyConflict
			}
			return recheck.settings, true, nil
		}
		return invoiceledger.Settings{}, false, invoiceledger.ErrConflict
	}
	var updated invoiceledger.Settings
	err = tx.QueryRow(ctx, updateSettingsSQL,
		update.ShopWarningMinor, update.ExternalPlanningMinor,
	).Scan(
		&updated.ShopWarningMinor, &updated.ExternalPlanningMinor,
		&updated.Version, &updated.UpdatedAt,
	)
	if err != nil {
		return invoiceledger.Settings{}, false, translate("update invoice settings", err)
	}
	updated.UpdatedAt = updated.UpdatedAt.UTC()
	if update.IdempotencyKey != uuid.Nil {
		_, err = tx.Exec(ctx, insertSettingsRequestSQL,
			update.IdempotencyKey, payloadHash, updated.ShopWarningMinor,
			updated.ExternalPlanningMinor, updated.Version, updated.UpdatedAt,
		)
		if err != nil {
			return invoiceledger.Settings{}, false, translate("record settings request", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return invoiceledger.Settings{}, false, translate("commit settings transaction", err)
	}
	return updated, false, nil
}

func readSettingsRequestTx(
	ctx context.Context,
	tx pgx.Tx,
	key uuid.UUID,
) (settingsRequestRow, bool, error) {
	if key == uuid.Nil {
		return settingsRequestRow{}, false, nil
	}
	var row settingsRequestRow
	err := tx.QueryRow(ctx, `
		SELECT payload_hash, shop_warning_minor, external_planning_minor,
		       version, settings_updated_at
		FROM shop.invoice_settings_requests
		WHERE idempotency_key = $1`, key).Scan(
		&row.payloadHash, &row.settings.ShopWarningMinor,
		&row.settings.ExternalPlanningMinor, &row.settings.Version,
		&row.settings.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return settingsRequestRow{}, false, nil
	}
	if err != nil {
		return settingsRequestRow{}, false, translate("read settings request", err)
	}
	row.settings.UpdatedAt = row.settings.UpdatedAt.UTC()
	return row, true, nil
}
