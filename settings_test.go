//nolint:testpackage // White-box tests use the shared Service fixture and the MemoryStore internals.
package invoiceledger

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestGetSettingsDefaults(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	settings, err := f.service.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if settings.ShopWarningMinor != DefaultShopWarningMinor ||
		settings.ExternalPlanningMinor != DefaultExternalPlanningMinor ||
		settings.Version != 1 || settings.UpdatedAt.IsZero() {
		t.Fatalf("default settings = %+v", settings)
	}
}

func TestUpdateSettingsAndReplay(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	ctx := context.Background()
	key := uuid.New()
	update := SettingsUpdate{
		ShopWarningMinor: 5_000, ExternalPlanningMinor: 7_000,
		Version: 1, IdempotencyKey: key,
	}
	settings, replayed, err := f.service.UpdateSettings(ctx, update)
	if err != nil || replayed {
		t.Fatalf("update settings = %+v replayed=%t err=%v", settings, replayed, err)
	}
	if settings.ShopWarningMinor != 5_000 || settings.ExternalPlanningMinor != 7_000 ||
		settings.Version != 2 {
		t.Fatalf("updated settings = %+v", settings)
	}
	replay, replayed, err := f.service.UpdateSettings(ctx, update)
	if err != nil || !replayed || replay != settings {
		t.Fatalf("replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
	conflict := update
	conflict.ShopWarningMinor = 5_001
	if _, _, err := f.service.UpdateSettings(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("payload conflict error = %v", err)
	}
	stale := SettingsUpdate{
		ShopWarningMinor: 6_000, ExternalPlanningMinor: 8_000, Version: 1,
		IdempotencyKey: uuid.New(),
	}
	if _, _, err := f.service.UpdateSettings(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version error = %v", err)
	}
	next := SettingsUpdate{
		ShopWarningMinor: 6_000, ExternalPlanningMinor: 8_000, Version: 2,
		IdempotencyKey: uuid.New(),
	}
	applied, replayed, err := f.service.UpdateSettings(ctx, next)
	if err != nil || replayed || applied.Version != 3 {
		t.Fatalf("next update = %+v replayed=%t err=%v", applied, replayed, err)
	}
	current, err := f.service.GetSettings(ctx)
	if err != nil || current.Version != 3 || current.ShopWarningMinor != 6_000 {
		t.Fatalf("current settings = %+v, %v", current, err)
	}
}

func TestUpdateSettingsReplaySurvivesLaterUpdates(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	ctx := context.Background()
	firstKey := uuid.New()
	first := SettingsUpdate{
		ShopWarningMinor: 1_000, ExternalPlanningMinor: 1_000,
		Version: 1, IdempotencyKey: firstKey,
	}
	firstSettings, _, err := f.service.UpdateSettings(ctx, first)
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	second := SettingsUpdate{
		ShopWarningMinor: 2_000, ExternalPlanningMinor: 2_000,
		Version: 2, IdempotencyKey: uuid.New(),
	}
	if _, _, err := f.service.UpdateSettings(ctx, second); err != nil {
		t.Fatalf("second update: %v", err)
	}
	replay, replayed, err := f.service.UpdateSettings(ctx, first)
	if err != nil || !replayed || replay != firstSettings {
		t.Fatalf("replayed outcome = %+v replayed=%t err=%v", replay, replayed, err)
	}
}

func TestUpdateSettingsKeylessAndValidation(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	ctx := context.Background()
	first := SettingsUpdate{ShopWarningMinor: 10, ExternalPlanningMinor: 20, Version: 1}
	if _, replayed, err := f.service.UpdateSettings(ctx, first); err != nil || replayed {
		t.Fatalf("keyless first update = replayed=%t err=%v", replayed, err)
	}
	second := SettingsUpdate{ShopWarningMinor: 11, ExternalPlanningMinor: 21, Version: 2}
	settings, replayed, err := f.service.UpdateSettings(ctx, second)
	if err != nil || replayed || settings.Version != 3 {
		t.Fatalf("keyless second update = %+v replayed=%t err=%v", settings, replayed, err)
	}
	for name, update := range map[string]SettingsUpdate{
		"negative warning":  {ShopWarningMinor: -1, Version: 3},
		"negative planning": {ExternalPlanningMinor: -1, Version: 3},
		"zero version":      {Version: 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, updateErr := f.service.UpdateSettings(ctx, update); !errors.Is(updateErr, ErrUnprocessable) {
				t.Fatalf("error = %v, want ErrUnprocessable", updateErr)
			}
		})
	}
}

func TestSettingsUnsupportedStore(t *testing.T) {
	t.Parallel()

	service, err := New(Options{
		Source: &memorySource{},
		Store:  storeOnly{Store: NewMemoryStore()},
		Seller: testSeller(),
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	ctx := context.Background()
	if _, err := service.GetSettings(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("get error = %v", err)
	}
	if _, _, err := service.UpdateSettings(ctx, SettingsUpdate{Version: 1}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("update error = %v", err)
	}
}

func TestConcurrentSettingsSameKeyAppliesOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	ctx := context.Background()
	key := uuid.New()
	update := SettingsUpdate{
		ShopWarningMinor: 3_000, ExternalPlanningMinor: 4_000,
		Version: 1, IdempotencyKey: key,
	}
	const workers = 8
	type outcome struct {
		replayed bool
		version  int64
		err      error
	}
	results := make(chan outcome, workers)
	var waitGroup sync.WaitGroup
	for range workers {
		waitGroup.Go(func() {
			settings, replayed, err := f.service.UpdateSettings(ctx, update)
			results <- outcome{replayed: replayed, version: settings.Version, err: err}
		})
	}
	waitGroup.Wait()
	close(results)
	applied := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent update error = %v", result.err)
		}
		if result.version != 2 {
			t.Fatalf("version = %d, want 2", result.version)
		}
		if !result.replayed {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("applied updates = %d, want 1", applied)
	}
}
