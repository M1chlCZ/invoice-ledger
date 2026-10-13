package invoiceledger

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"
)

const (
	// DefaultShopWarningMinor is the default internal warning budget for net
	// invoiced turnover in minor units.
	DefaultShopWarningMinor int64 = 100_000_000

	// DefaultExternalPlanningMinor is the default planning assumption for
	// external turnover in minor units.
	DefaultExternalPlanningMinor int64 = 100_000_000
)

// Settings is the singleton invoicing monitoring configuration. The shop
// warning budget and the external planning assumption are not legal turnover
// thresholds. Numbering lives in the Store sequence series, not in Settings.
type Settings struct {
	ShopWarningMinor      int64     `json:"shop_warning_minor"`
	ExternalPlanningMinor int64     `json:"external_planning_minor"`
	Version               int64     `json:"version"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// SettingsUpdate updates the settings singleton with optimistic versioning.
// A non-nil IdempotencyKey makes the update replay-safe.
type SettingsUpdate struct {
	ShopWarningMinor      int64
	ExternalPlanningMinor int64
	Version               int64
	IdempotencyKey        uuid.UUID
}

// GetSettings returns the current invoicing settings singleton. The configured
// Store must implement SettingsStore or GetSettings returns ErrUnsupported.
func (service *Service) GetSettings(ctx context.Context) (Settings, error) {
	store, ok := service.store.(SettingsStore)
	if !ok {
		return Settings{}, ErrUnsupported
	}
	return store.GetSettings(ctx)
}

// UpdateSettings replays or applies one version-checked settings update. The
// thresholds must be non-negative and the expected version must be at least
// one, or UpdateSettings returns ErrUnprocessable. The configured Store must
// implement SettingsStore or UpdateSettings returns ErrUnsupported.
//
// A repeated key with the same payload returns the recorded outcome with
// replayed true, even after later updates changed the live singleton. The same
// key with a different payload returns ErrIdempotencyConflict. A stale version
// returns ErrConflict.
func (service *Service) UpdateSettings(
	ctx context.Context,
	update SettingsUpdate,
) (Settings, bool, error) {
	if update.ShopWarningMinor < 0 || update.ExternalPlanningMinor < 0 || update.Version < 1 {
		return Settings{}, false, ErrUnprocessable
	}
	store, ok := service.store.(SettingsStore)
	if !ok {
		return Settings{}, false, ErrUnsupported
	}
	hash := payloadHash(
		"settings", strconv.FormatInt(update.ShopWarningMinor, 10),
		strconv.FormatInt(update.ExternalPlanningMinor, 10),
		strconv.FormatInt(update.Version, 10),
	)
	return store.UpdateSettings(ctx, update, hash)
}
