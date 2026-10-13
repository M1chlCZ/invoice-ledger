package invoiceledger

import (
	"bytes"
	"context"
	"time"

	"github.com/google/uuid"
)

// GetSettings implements SettingsStore.
func (store *MemoryStore) GetSettings(_ context.Context) (Settings, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.settings, nil
}

// UpdateSettings implements SettingsStore. The replay lookup, the version
// check and the apply run under one lock, so concurrent and cross-process
// writers that share this store cannot double-apply one update.
func (store *MemoryStore) UpdateSettings(
	_ context.Context,
	update SettingsUpdate,
	payloadHash []byte,
) (Settings, bool, error) {
	if update.ShopWarningMinor < 0 || update.ExternalPlanningMinor < 0 || update.Version < 1 {
		return Settings{}, false, ErrUnprocessable
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if update.IdempotencyKey != uuid.Nil {
		if record, exists := store.settingsKeys[update.IdempotencyKey]; exists {
			if !bytes.Equal(record.payloadHash, payloadHash) {
				return Settings{}, false, ErrIdempotencyConflict
			}
			return record.settings, true, nil
		}
	}
	if store.settings.Version != update.Version {
		return Settings{}, false, ErrConflict
	}
	store.settings.ShopWarningMinor = update.ShopWarningMinor
	store.settings.ExternalPlanningMinor = update.ExternalPlanningMinor
	store.settings.Version++
	store.settings.UpdatedAt = time.Now().UTC()
	updated := store.settings
	if update.IdempotencyKey != uuid.Nil {
		store.settingsKeys[update.IdempotencyKey] = settingsRequest{
			payloadHash: append([]byte(nil), payloadHash...),
			settings:    updated,
		}
	}
	return updated, false, nil
}
