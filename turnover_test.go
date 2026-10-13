//nolint:testpackage // White-box tests use the shared Service fixture and the MemoryStore internals.
package invoiceledger

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testTurnoverDate = "2026-03-10"

type storeOnly struct {
	Store
}

func newTurnoverFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixture(t, nil)
}

func TestCreateTurnoverAndReplay(t *testing.T) {
	t.Parallel()

	f := newTurnoverFixture(t)
	ctx := context.Background()
	key := uuid.New()
	request := TurnoverRequest{
		OccurredOn:     testTurnoverDate,
		AmountMinor:    250_000,
		CountryCode:    "DE",
		Classification: TurnoverEUTBE,
		Note:           "marketplace payout",
		IdempotencyKey: key,
	}
	entry, replayed, err := f.service.CreateTurnover(ctx, request)
	if err != nil || replayed {
		t.Fatalf("create turnover = %+v replayed=%t err=%v", entry, replayed, err)
	}
	if entry.ID == uuid.Nil || entry.CreatedAt.IsZero() {
		t.Fatalf("create did not assign id and created time: %+v", entry)
	}
	if entry.OccurredOn != testTurnoverDate || entry.AmountMinor != 250_000 ||
		entry.CountryCode != "DE" || entry.Classification != TurnoverEUTBE ||
		entry.Note != "marketplace payout" || entry.ReversalOf != nil ||
		entry.Reason != "" {
		t.Fatalf("create fields = %+v", entry)
	}
	if entry.IdempotencyKey == nil || *entry.IdempotencyKey != key ||
		len(entry.IdempotencyPayloadHash) == 0 {
		t.Fatalf("create idempotency = %+v", entry)
	}
	replay, replayed, err := f.service.CreateTurnover(ctx, request)
	if err != nil || !replayed || replay.ID != entry.ID {
		t.Fatalf("replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
	conflict := request
	conflict.AmountMinor = 250_001
	if _, _, err := f.service.CreateTurnover(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("payload conflict error = %v", err)
	}
	keyless, replayed, err := f.service.CreateTurnover(ctx, TurnoverRequest{
		OccurredOn: testTurnoverDate, AmountMinor: 1_000,
		Classification: TurnoverDomestic,
	})
	if err != nil || replayed || keyless.IdempotencyKey != nil {
		t.Fatalf("keyless create = %+v replayed=%t err=%v", keyless, replayed, err)
	}
}

func TestCreateTurnoverValidatesInput(t *testing.T) {
	t.Parallel()

	f := newTurnoverFixture(t)
	ctx := context.Background()
	valid := TurnoverRequest{
		OccurredOn: testTurnoverDate, AmountMinor: 1_000,
		Classification: TurnoverPending,
	}
	cases := map[string]func(*TurnoverRequest){
		"missing date":      func(request *TurnoverRequest) { request.OccurredOn = "" },
		"invalid date":      func(request *TurnoverRequest) { request.OccurredOn = "2026-02-30" },
		"short date":        func(request *TurnoverRequest) { request.OccurredOn = "2026-3-1" },
		"zero amount":       func(request *TurnoverRequest) { request.AmountMinor = 0 },
		"negative amount":   func(request *TurnoverRequest) { request.AmountMinor = -1 },
		"unknown class":     func(request *TurnoverRequest) { request.Classification = "other" },
		"empty class":       func(request *TurnoverRequest) { request.Classification = "" },
		"lowercase country": func(request *TurnoverRequest) { request.CountryCode = "cz" },
		"long country":      func(request *TurnoverRequest) { request.CountryCode = "CZE" },
		"digit country":     func(request *TurnoverRequest) { request.CountryCode = "C1" },
		"oversized note":    func(request *TurnoverRequest) { request.Note = strings.Repeat("a", reasonMaxRunes+1) },
		"invalid utf8 note": func(request *TurnoverRequest) { request.Note = "\xff" },
	}
	for name, customize := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			request := valid
			customize(&request)
			if _, _, err := f.service.CreateTurnover(ctx, request); !errors.Is(err, ErrUnprocessable) {
				t.Fatalf("error = %v, want ErrUnprocessable", err)
			}
		})
	}
	edge := valid
	edge.CountryCode = "CZ"
	edge.Note = strings.Repeat("a", reasonMaxRunes)
	if _, _, err := f.service.CreateTurnover(ctx, edge); err != nil {
		t.Fatalf("edge create error = %v", err)
	}
}

func TestVoidTurnover(t *testing.T) {
	t.Parallel()

	f := newTurnoverFixture(t)
	ctx := context.Background()
	target, _, err := f.service.CreateTurnover(ctx, TurnoverRequest{
		OccurredOn: testTurnoverDate, AmountMinor: 120_000, CountryCode: "SK",
		Classification: TurnoverEUTBE, Note: "external invoice 7",
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	key := uuid.New()
	reversal, replayed, err := f.service.VoidTurnover(ctx, TurnoverVoidRequest{
		EntryID: target.ID, Reason: "duplicate entry", IdempotencyKey: key,
	})
	if err != nil || replayed {
		t.Fatalf("void = %+v replayed=%t err=%v", reversal, replayed, err)
	}
	if reversal.ReversalOf == nil || *reversal.ReversalOf != target.ID ||
		reversal.AmountMinor != -120_000 || reversal.Reason != "duplicate entry" ||
		reversal.OccurredOn != target.OccurredOn ||
		reversal.CountryCode != target.CountryCode ||
		reversal.Classification != target.Classification ||
		reversal.Note != target.Note {
		t.Fatalf("reversal fields = %+v", reversal)
	}
	replay, replayed, err := f.service.VoidTurnover(ctx, TurnoverVoidRequest{
		EntryID: target.ID, Reason: "duplicate entry", IdempotencyKey: key,
	})
	if err != nil || !replayed || replay.ID != reversal.ID {
		t.Fatalf("void replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
	if _, _, err := f.service.VoidTurnover(ctx, TurnoverVoidRequest{
		EntryID: target.ID, Reason: "another reason", IdempotencyKey: key,
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("void payload conflict error = %v", err)
	}
	if _, _, err := f.service.VoidTurnover(ctx, TurnoverVoidRequest{
		EntryID: target.ID, Reason: "void again", IdempotencyKey: uuid.New(),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("double void error = %v", err)
	}
	if _, _, err := f.service.VoidTurnover(ctx, TurnoverVoidRequest{
		EntryID: reversal.ID, Reason: "void a reversal", IdempotencyKey: uuid.New(),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("void of reversal error = %v", err)
	}
	if _, _, err := f.service.VoidTurnover(ctx, TurnoverVoidRequest{
		EntryID: uuid.New(), Reason: "missing", IdempotencyKey: uuid.New(),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing target error = %v", err)
	}
	if _, _, err := f.service.VoidTurnover(ctx, TurnoverVoidRequest{
		EntryID: target.ID, Reason: "   ", IdempotencyKey: uuid.New(),
	}); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("empty reason error = %v", err)
	}
}

func TestMemoryTurnoverByYearOrderingAndDerivation(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	ctx := context.Background()
	early := TurnoverEntry{
		ID:         uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		OccurredOn: "2025-12-31", AmountMinor: 100, Classification: TurnoverDomestic,
		CreatedAt: time.Date(2025, 12, 31, 10, 0, 0, 0, time.UTC),
	}
	late := TurnoverEntry{
		ID:         uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		OccurredOn: testTurnoverDate, AmountMinor: 200, Classification: TurnoverEUTBE,
		CreatedAt: time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC),
	}
	sameTime := TurnoverEntry{
		ID:         uuid.MustParse("00000000-0000-0000-0000-000000000003"),
		OccurredOn: testTurnoverDate, AmountMinor: 300, Classification: TurnoverPending,
		CreatedAt: time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC),
	}
	for _, entry := range []TurnoverEntry{early, late, sameTime} {
		if _, err := store.InsertTurnover(ctx, entry); err != nil {
			t.Fatalf("insert %s: %v", entry.ID, err)
		}
	}
	year, err := store.TurnoverByYear(ctx, 2026)
	if err != nil || len(year) != 2 {
		t.Fatalf("year entries = %+v, %v", year, err)
	}
	if year[0].ID != late.ID || year[1].ID != sameTime.ID {
		t.Fatalf("year order = %s, %s", year[0].ID, year[1].ID)
	}
	previous, err := store.TurnoverByYear(ctx, 2025)
	if err != nil || len(previous) != 1 || previous[0].ID != early.ID {
		t.Fatalf("previous year = %+v, %v", previous, err)
	}
	all, err := store.TurnoverByYear(ctx, 0)
	if err != nil || len(all) != 3 || all[0].ID != late.ID || all[1].ID != sameTime.ID ||
		all[2].ID != early.ID {
		t.Fatalf("all entries = %+v, %v", all, err)
	}
	if _, err := store.TurnoverByYear(ctx, -1); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("negative year error = %v", err)
	}
	reversal, err := store.InsertTurnoverReversal(ctx, TurnoverEntry{
		ReversalOf:  &late.ID,
		Reason:      "correction",
		OccurredOn:  "1999-01-01",
		AmountMinor: 7,
	})
	if err != nil {
		t.Fatalf("reversal: %v", err)
	}
	if reversal.AmountMinor != -200 || reversal.OccurredOn != late.OccurredOn ||
		reversal.CountryCode != late.CountryCode ||
		reversal.Classification != late.Classification ||
		reversal.Note != late.Note || reversal.Reason != "correction" {
		t.Fatalf("derived reversal = %+v", reversal)
	}
	if _, err := store.InsertTurnover(ctx, TurnoverEntry{
		OccurredOn: testTurnoverDate, AmountMinor: 5, Classification: TurnoverExcluded,
		ReversalOf: &late.ID,
	}); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("create with reversal fields error = %v", err)
	}
	if _, err := store.InsertTurnoverReversal(ctx, TurnoverEntry{
		ReversalOf: &early.ID, Reason: "second",
	}); err != nil {
		t.Fatalf("second reversal: %v", err)
	}
	if _, err := store.InsertTurnoverReversal(ctx, TurnoverEntry{
		ReversalOf: &early.ID, Reason: "again",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("already reversed error = %v", err)
	}
	fetched, err := store.TurnoverByID(ctx, reversal.ID)
	if err != nil || fetched.AmountMinor != reversal.AmountMinor {
		t.Fatalf("fetch reversal = %+v, %v", fetched, err)
	}
	if _, err := store.TurnoverByID(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id error = %v", err)
	}
}

func TestTurnoverUnsupportedStore(t *testing.T) {
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
	if _, _, err := service.CreateTurnover(ctx, TurnoverRequest{
		OccurredOn: testTurnoverDate, AmountMinor: 1, Classification: TurnoverDomestic,
	}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("create error = %v", err)
	}
	if _, err := service.ListTurnover(ctx, 2026); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("list error = %v", err)
	}
}

func TestConcurrentCreateTurnoverSameKeyReplaysOnce(t *testing.T) {
	t.Parallel()

	f := newTurnoverFixture(t)
	ctx := context.Background()
	key := uuid.New()
	request := TurnoverRequest{
		OccurredOn: testTurnoverDate, AmountMinor: 42_000,
		Classification: TurnoverDomestic, IdempotencyKey: key,
	}
	const workers = 8
	type outcome struct {
		entry    TurnoverEntry
		replayed bool
		err      error
	}
	results := make(chan outcome, workers)
	var waitGroup sync.WaitGroup
	for range workers {
		waitGroup.Go(func() {
			entry, replayed, err := f.service.CreateTurnover(ctx, request)
			results <- outcome{entry: entry, replayed: replayed, err: err}
		})
	}
	waitGroup.Wait()
	close(results)
	var firstID uuid.UUID
	replays := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent create error = %v", result.err)
		}
		if firstID == uuid.Nil {
			firstID = result.entry.ID
		}
		if result.entry.ID != firstID {
			t.Fatalf("concurrent ids = %s, want %s", result.entry.ID, firstID)
		}
		if result.replayed {
			replays++
		}
	}
	if replays != workers-1 {
		t.Fatalf("replays = %d, want %d", replays, workers-1)
	}
}

func TestConcurrentVoidTurnoverReversesOnce(t *testing.T) {
	t.Parallel()

	f := newTurnoverFixture(t)
	ctx := context.Background()
	target, _, err := f.service.CreateTurnover(ctx, TurnoverRequest{
		OccurredOn: testTurnoverDate, AmountMinor: 9_000, Classification: TurnoverDomestic,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	const workers = 4
	results := make(chan error, workers)
	var waitGroup sync.WaitGroup
	for index := range workers {
		waitGroup.Go(func() {
			_, _, voidErr := f.service.VoidTurnover(ctx, TurnoverVoidRequest{
				EntryID: target.ID, Reason: strings.Repeat("v", index+1),
				IdempotencyKey: uuid.New(),
			})
			results <- voidErr
		})
	}
	waitGroup.Wait()
	close(results)
	accepted := 0
	for voidErr := range results {
		switch {
		case voidErr == nil:
			accepted++
		case errors.Is(voidErr, ErrConflict):
		default:
			t.Fatalf("concurrent void error = %v", voidErr)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted voids = %d, want 1", accepted)
	}
	entries, err := f.service.ListTurnover(ctx, 2026)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries after concurrent void = %d, %v", len(entries), err)
	}
}
