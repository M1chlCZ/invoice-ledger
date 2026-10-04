//nolint:testpackage // White-box tests exercise the unexported remainingCredit helper and Service internals.
package invoiceledger

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
)

type conflictOnceStore struct {
	Store

	conflicts int
}

func (store *conflictOnceStore) Insert(ctx context.Context, document Document) (Document, error) {
	stored, err := store.Store.Insert(ctx, document)
	if err != nil {
		return Document{}, err
	}
	if store.conflicts > 0 {
		store.conflicts--
		return Document{}, ErrConflict
	}
	return stored, nil
}

type errorStore struct {
	*MemoryStore

	allocateErr error
	insertErr   error
	byIDErr     error
	byOrderErr  error
	byKeyErr    error
	creditedErr error
}

func (store errorStore) AllocateNumber(ctx context.Context, year int, kind Kind) (Sequence, error) {
	if store.allocateErr != nil {
		return Sequence{}, store.allocateErr
	}
	return store.MemoryStore.AllocateNumber(ctx, year, kind)
}

func (store errorStore) Insert(ctx context.Context, document Document) (Document, error) {
	if store.insertErr != nil {
		return Document{}, store.insertErr
	}
	return store.MemoryStore.Insert(ctx, document)
}

func (store errorStore) ByID(ctx context.Context, id uuid.UUID) (Document, error) {
	if store.byIDErr != nil {
		return Document{}, store.byIDErr
	}
	return store.MemoryStore.ByID(ctx, id)
}

func (store errorStore) ByOrder(ctx context.Context, orderID uuid.UUID) ([]Document, error) {
	if store.byOrderErr != nil {
		return nil, store.byOrderErr
	}
	return store.MemoryStore.ByOrder(ctx, orderID)
}

func (store errorStore) ByIdempotencyKey(ctx context.Context, key uuid.UUID) (Document, error) {
	if store.byKeyErr != nil {
		return Document{}, store.byKeyErr
	}
	return store.MemoryStore.ByIdempotencyKey(ctx, key)
}

func (store errorStore) CreditedTotal(ctx context.Context, originalID uuid.UUID) (int64, error) {
	if store.creditedErr != nil {
		return 0, store.creditedErr
	}
	return store.MemoryStore.CreditedTotal(ctx, originalID)
}

func TestIssueWithoutIdempotencyKey(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, nil, facts)
	ctx := context.Background()
	document, replayed, err := f.service.Issue(ctx, IssueRequest{OrderID: facts.OrderID})
	if err != nil || replayed || document.IdempotencyKey != nil || len(document.IdempotencyPayloadHash) == 0 {
		t.Fatalf("keyless issue = %+v replayed=%t err=%v", document, replayed, err)
	}
	replay, replayed, err := f.service.Issue(ctx, IssueRequest{OrderID: facts.OrderID})
	if err != nil || !replayed || replay.ID != document.ID {
		t.Fatalf("keyless replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
}

func TestRemainingCreditGuards(t *testing.T) {
	t.Parallel()

	if remaining, ok := remainingCredit(1000, -700); !ok || remaining != 300 {
		t.Fatalf("partial remaining = %d, %v", remaining, ok)
	}
	if remaining, ok := remainingCredit(math.MaxInt64, 1); ok || remaining != 0 {
		t.Fatalf("positive overflow guard = %d, %v", remaining, ok)
	}
	if remaining, ok := remainingCredit(math.MinInt64, -1); ok || remaining != 0 {
		t.Fatalf("negative overflow guard = %d, %v", remaining, ok)
	}
}

func TestIssueReplaysThroughInsertConflict(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, func(options *Options) {
		options.Store = &conflictOnceStore{Store: options.Store, conflicts: 1}
	}, facts)
	ctx := context.Background()
	key := uuid.New()
	document, _, err := f.service.Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: key,
	})
	if err != nil || document.Number == "" {
		t.Fatalf("first issue = %+v err=%v", document, err)
	}
	replay, replayed, err := f.service.Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: key,
	})
	if err != nil || !replayed || replay.ID != document.ID {
		t.Fatalf("conflict replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
}

func TestCreditReplaysThroughInsertConflict(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	conflicting := &conflictOnceStore{Store: NewMemoryStore()}
	f := newFixture(t, func(options *Options) {
		options.Store = conflicting
	}, facts)
	ctx := context.Background()
	document := f.issue(facts.OrderID, uuid.New())
	conflicting.conflicts = 1
	key := uuid.New()
	request := CreditRequest{
		InvoiceID: document.ID, AmountMinor: 300, Reason: "partial refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: key,
	}
	credit, _, err := f.service.Credit(ctx, request)
	if err != nil || credit.Number == "" {
		t.Fatalf("first credit = %+v err=%v", credit, err)
	}
	replay, replayed, err := f.service.Credit(ctx, request)
	if err != nil || !replayed || replay.ID != credit.ID {
		t.Fatalf("conflict replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
}

type rejectFirstStore struct {
	Store

	rejections int
}

func (store *rejectFirstStore) Insert(ctx context.Context, document Document) (Document, error) {
	if store.rejections > 0 {
		store.rejections--
		return Document{}, ErrConflict
	}
	return store.Store.Insert(ctx, document)
}

func TestInsertConflictWithoutStoredDocument(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	source := &memorySource{orders: map[uuid.UUID]OrderFacts{facts.OrderID: facts}}
	ctx := context.Background()
	rejecting := &rejectFirstStore{Store: NewMemoryStore(), rejections: 1}
	issuing, err := New(Options{Source: source, Store: rejecting, Seller: testSeller()})
	if err != nil {
		t.Fatalf("new issuing service: %v", err)
	}
	if _, _, issueErr := issuing.Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: uuid.New(),
	}); !errors.Is(issueErr, ErrConflict) {
		t.Fatalf("issue conflict error = %v", issueErr)
	}
	base := NewMemoryStore()
	seeding, err := New(Options{Source: source, Store: base, Seller: testSeller()})
	if err != nil {
		t.Fatalf("new seeding service: %v", err)
	}
	document, _, err := seeding.Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: uuid.New(),
	})
	if err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	creditRejecting := &rejectFirstStore{Store: base, rejections: 1}
	crediting, err := New(Options{Source: source, Store: creditRejecting, Seller: testSeller()})
	if err != nil {
		t.Fatalf("new crediting service: %v", err)
	}
	if _, _, creditErr := crediting.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 100, Reason: "refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	}); !errors.Is(creditErr, ErrConflict) {
		t.Fatalf("credit conflict error = %v", creditErr)
	}
}

type positiveCreditedStore struct {
	*MemoryStore
}

func (store positiveCreditedStore) CreditedTotal(context.Context, uuid.UUID) (int64, error) {
	return 1, nil
}

func TestCreditRejectsPositiveCreditedTotal(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, func(options *Options) {
		options.Store = positiveCreditedStore{MemoryStore: NewMemoryStore()}
	}, facts)
	ctx := context.Background()
	document := f.issue(facts.OrderID, uuid.New())
	if _, _, err := f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 100, Reason: "refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("positive credited total error = %v", err)
	}
}

func TestMemoryStoreInsertCreditGuards(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	ctx := context.Background()
	original := uuid.New()
	credit := Document{
		Kind: KindCreditNote, SequenceYear: 2026, SequenceNumber: 1,
		Number: "OP-2026-000001", TotalMinor: -100,
	}
	if _, err := store.InsertCredit(ctx, credit, original, -1); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("negative limit error = %v", err)
	}
	notCredit := credit
	notCredit.Kind = KindInvoice
	if _, err := store.InsertCredit(ctx, notCredit, original, 100); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("non-credit document error = %v", err)
	}
	positive := credit
	positive.TotalMinor = 100
	if _, err := store.InsertCredit(ctx, positive, original, 100); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("positive credit total error = %v", err)
	}
	seeded := Document{
		Kind: KindCreditNote, SequenceYear: 2026, SequenceNumber: 99,
		Number: "OP-2026-000099", TotalMinor: math.MinInt64, OriginalID: &original,
	}
	if _, err := store.Insert(ctx, seeded); err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	if _, err := store.InsertCredit(ctx, credit, original, math.MaxInt64); !errors.Is(err, ErrConflict) {
		t.Fatalf("credited total overflow error = %v", err)
	}

	overflowing := NewMemoryStore()
	firstSeed := Document{
		Kind: KindCreditNote, SequenceYear: 2026, SequenceNumber: 1,
		Number: "OP-2026-000001", TotalMinor: math.MaxInt64, OriginalID: &original,
	}
	secondSeed := Document{
		Kind: KindCreditNote, SequenceYear: 2026, SequenceNumber: 2,
		Number: "OP-2026-000002", TotalMinor: 1, OriginalID: &original,
	}
	if _, err := overflowing.Insert(ctx, firstSeed); err != nil {
		t.Fatalf("seed positive credit: %v", err)
	}
	if _, err := overflowing.Insert(ctx, secondSeed); err != nil {
		t.Fatalf("seed second positive credit: %v", err)
	}
	if _, err := overflowing.InsertCredit(ctx, credit, original, math.MaxInt64); !errors.Is(err, ErrConflict) {
		t.Fatalf("positive credited total overflow error = %v", err)
	}

	duplicates := NewMemoryStore()
	valid := credit
	valid.OriginalID = &original
	if _, err := duplicates.InsertCredit(ctx, valid, original, 1000); err != nil {
		t.Fatalf("valid InsertCredit: %v", err)
	}
	if _, err := duplicates.InsertCredit(ctx, valid, original, 1000); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate InsertCredit error = %v", err)
	}
}

type recordingCreditStore struct {
	*MemoryStore

	insertCalls       int
	insertCreditCalls int
}

func (store *recordingCreditStore) Insert(ctx context.Context, document Document) (Document, error) {
	store.insertCalls++
	return store.MemoryStore.Insert(ctx, document)
}

func (store *recordingCreditStore) InsertCredit(
	ctx context.Context,
	document Document,
	originalID uuid.UUID,
	maxTotalMinor int64,
) (Document, error) {
	store.insertCreditCalls++
	return store.MemoryStore.InsertCredit(ctx, document, originalID, maxTotalMinor)
}

func TestCreditPrefersCreditInserter(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	store := &recordingCreditStore{MemoryStore: NewMemoryStore()}
	f := newFixture(t, func(options *Options) {
		options.Store = store
	}, facts)
	ctx := context.Background()
	document := f.issue(facts.OrderID, uuid.New())
	store.insertCalls = 0
	if _, _, err := f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 300, Reason: "partial refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	}); err != nil {
		t.Fatalf("credit: %v", err)
	}
	if store.insertCreditCalls != 1 || store.insertCalls != 0 {
		t.Fatalf("insertCredit calls = %d, insert calls = %d, want 1 and 0",
			store.insertCreditCalls, store.insertCalls)
	}
}

type hideKeyOnceStore struct {
	Store

	hidden int
}

func (store *hideKeyOnceStore) ByIdempotencyKey(ctx context.Context, key uuid.UUID) (Document, error) {
	if store.hidden > 0 {
		store.hidden--
		return Document{}, ErrNotFound
	}
	return store.Store.ByIdempotencyKey(ctx, key)
}

type conflictKeyStore struct {
	*MemoryStore

	conflicts int
	byKeyErr  error
}

func (store *conflictKeyStore) Insert(ctx context.Context, document Document) (Document, error) {
	stored, err := store.MemoryStore.Insert(ctx, document)
	if err != nil {
		return Document{}, err
	}
	if store.conflicts > 0 {
		store.conflicts--
		return Document{}, ErrConflict
	}
	return stored, nil
}

func (store *conflictKeyStore) ByIdempotencyKey(ctx context.Context, key uuid.UUID) (Document, error) {
	if store.byKeyErr != nil {
		return Document{}, store.byKeyErr
	}
	return store.MemoryStore.ByIdempotencyKey(ctx, key)
}

func TestIssueReplayFallsBackToOrderLookup(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	conflicting := &conflictOnceStore{Store: NewMemoryStore(), conflicts: 1}
	hiding := &hideKeyOnceStore{Store: conflicting, hidden: 1}
	f := newFixture(t, func(options *Options) {
		options.Store = hiding
	}, facts)
	document, replayed, err := f.service.Issue(context.Background(), IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: uuid.New(),
	})
	if err != nil || !replayed || document.Number == "" {
		t.Fatalf("fallback replay = %+v replayed=%t err=%v", document, replayed, err)
	}
}

func TestIssueReplayReportsKeyLookupFailure(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	storeErr := errors.New("key lookup down")
	conflicting := &conflictKeyStore{
		MemoryStore: NewMemoryStore(), conflicts: 1, byKeyErr: storeErr,
	}
	f := newFixture(t, func(options *Options) {
		options.Store = conflicting
	}, facts)
	if _, _, err := f.service.Issue(context.Background(), IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: uuid.New(),
	}); !errors.Is(err, storeErr) {
		t.Fatalf("key lookup error = %v", err)
	}
}

func TestStoreFailurePropagation(t *testing.T) {
	t.Parallel()

	storeErr := errors.New("store down")
	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	source := &memorySource{orders: map[uuid.UUID]OrderFacts{facts.OrderID: facts}}
	newService := func(store Store) *Service {
		service, err := New(Options{Source: source, Store: store, Seller: testSeller()})
		if err != nil {
			t.Fatalf("new service: %v", err)
		}
		return service
	}
	ctx := context.Background()
	base := NewMemoryStore()

	if _, err := newService(
		errorStore{MemoryStore: base, byIDErr: storeErr},
	).Get(ctx, uuid.New()); !errors.Is(
		err,
		storeErr,
	) {
		t.Fatalf("get error = %v", err)
	}
	if _, err := newService(
		errorStore{MemoryStore: base, byOrderErr: storeErr},
	).ListByOrder(ctx, uuid.New()); !errors.Is(
		err,
		storeErr,
	) {
		t.Fatalf("list error = %v", err)
	}
	if _, _, err := newService(errorStore{MemoryStore: base, byOrderErr: storeErr}).Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: uuid.New(),
	}); !errors.Is(err, storeErr) {
		t.Fatalf("issue order lookup error = %v", err)
	}
	if _, _, err := newService(errorStore{MemoryStore: base, allocateErr: storeErr}).Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: uuid.New(),
	}); !errors.Is(err, storeErr) {
		t.Fatalf("issue allocation error = %v", err)
	}
	if _, _, err := newService(errorStore{MemoryStore: base, insertErr: storeErr}).Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: uuid.New(),
	}); !errors.Is(err, storeErr) {
		t.Fatalf("issue insert error = %v", err)
	}

	issuing := newService(base)
	document, _, err := issuing.Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: uuid.New(),
	})
	if err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	if _, _, creditErr := newService(errorStore{MemoryStore: base, byIDErr: storeErr}).Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 100, Reason: "refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	}); !errors.Is(creditErr, storeErr) {
		t.Fatalf("credit original error = %v", creditErr)
	}
	if _, _, creditErr := newService(errorStore{MemoryStore: base, creditedErr: storeErr}).Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 100, Reason: "refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	}); !errors.Is(creditErr, storeErr) {
		t.Fatalf("credit total error = %v", creditErr)
	}
	if _, _, creditErr := newService(errorStore{MemoryStore: base, byKeyErr: storeErr}).Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 100, Reason: "refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	}); !errors.Is(creditErr, storeErr) {
		t.Fatalf("credit key error = %v", creditErr)
	}
}

func TestKeylessCreditConflictPath(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	rejecting := &rejectFirstStore{Store: NewMemoryStore()}
	f := newFixture(t, func(options *Options) {
		options.Store = rejecting
	}, facts)
	ctx := context.Background()
	document := f.issue(facts.OrderID, uuid.New())
	rejecting.rejections = 1
	if _, _, err := f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 300, Reason: "partial refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("keyless credit conflict error = %v", err)
	}
	documents, err := f.service.ListByOrder(ctx, facts.OrderID)
	if err != nil || len(documents) != 1 {
		t.Fatalf("documents after keyless conflict = %d, %v", len(documents), err)
	}
}
