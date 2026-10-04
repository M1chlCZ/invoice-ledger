package invoiceledger

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	minSequenceYear = 2000
	maxSequenceYear = 9999
)

// MemoryStore is a mutex-protected in-memory Store for tests and small tools.
type MemoryStore struct {
	mu         sync.Mutex
	documents  []Document
	byID       map[uuid.UUID]int
	byNumber   map[string]struct{}
	bySequence map[sequenceKey]struct{}
	byKey      map[uuid.UUID]struct{}
	counters   map[int]int64
}

type sequenceKey struct {
	year   int
	number int64
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		byID:       make(map[uuid.UUID]int),
		byNumber:   make(map[string]struct{}),
		bySequence: make(map[sequenceKey]struct{}),
		byKey:      make(map[uuid.UUID]struct{}),
		counters:   make(map[int]int64),
	}
}

// AllocateNumber implements Store.
func (store *MemoryStore) AllocateNumber(_ context.Context, year int, kind Kind) (Sequence, error) {
	if year < minSequenceYear || year > maxSequenceYear {
		return Sequence{}, ErrUnprocessable
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	next := store.counters[year]
	if next == 0 {
		next = 1
	}
	store.counters[year] = next + 1
	return Sequence{Year: year, Kind: kind, Number: next}, nil
}

// Insert implements Store.
func (store *MemoryStore) Insert(_ context.Context, document Document) (Document, error) {
	stored := prepareDocument(document)
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.insertLocked(stored); err != nil {
		return Document{}, err
	}
	return cloneDocument(stored), nil
}

// InsertCredit implements CreditInserter. The over-credit check and the
// insert run under one lock, so concurrent and cross-process writers that
// share this store cannot over-credit an invoice.
func (store *MemoryStore) InsertCredit(
	_ context.Context,
	document Document,
	originalID uuid.UUID,
	maxTotalMinor int64,
) (Document, error) {
	if document.Kind != KindCreditNote || document.TotalMinor > 0 || maxTotalMinor < 0 {
		return Document{}, ErrUnprocessable
	}
	stored := prepareDocument(document)
	store.mu.Lock()
	defer store.mu.Unlock()
	var credited int64
	for _, existing := range store.documents {
		if existing.Kind != KindCreditNote ||
			existing.OriginalID == nil ||
			*existing.OriginalID != originalID {
			continue
		}
		next, ok := addInt64(credited, existing.TotalMinor)
		if !ok {
			return Document{}, ErrConflict
		}
		credited = next
	}
	projected, ok := addInt64(credited, stored.TotalMinor)
	if !ok || projected < -maxTotalMinor {
		return Document{}, ErrConflict
	}
	if err := store.insertLocked(stored); err != nil {
		return Document{}, err
	}
	return cloneDocument(stored), nil
}

func prepareDocument(document Document) Document {
	stored := cloneDocument(document)
	if stored.ID == uuid.Nil {
		stored.ID = uuid.New()
	}
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	}
	return stored
}

func (store *MemoryStore) insertLocked(stored Document) error {
	if err := store.conflictLocked(stored); err != nil {
		return err
	}
	store.documents = append(store.documents, stored)
	store.byID[stored.ID] = len(store.documents) - 1
	if stored.Number != "" {
		store.byNumber[stored.Number] = struct{}{}
	}
	store.bySequence[sequenceKey{year: stored.SequenceYear, number: stored.SequenceNumber}] = struct{}{}
	if stored.IdempotencyKey != nil {
		store.byKey[*stored.IdempotencyKey] = struct{}{}
	}
	return nil
}

func (store *MemoryStore) conflictLocked(stored Document) error {
	if _, exists := store.byID[stored.ID]; exists {
		return ErrConflict
	}
	if stored.Number != "" {
		if _, exists := store.byNumber[stored.Number]; exists {
			return ErrConflict
		}
	}
	sequence := sequenceKey{year: stored.SequenceYear, number: stored.SequenceNumber}
	if _, exists := store.bySequence[sequence]; exists {
		return ErrConflict
	}
	if store.invoiceOrderConflictLocked(stored) {
		return ErrConflict
	}
	if stored.IdempotencyKey != nil {
		if _, exists := store.byKey[*stored.IdempotencyKey]; exists {
			return ErrConflict
		}
	}
	return nil
}

func (store *MemoryStore) invoiceOrderConflictLocked(stored Document) bool {
	if stored.Kind != KindInvoice || stored.OrderID == nil {
		return false
	}
	for _, existing := range store.documents {
		if existing.Kind != KindInvoice || existing.OrderID == nil {
			continue
		}
		if *existing.OrderID == *stored.OrderID {
			return true
		}
	}
	return false
}

// ByID implements Store.
func (store *MemoryStore) ByID(_ context.Context, id uuid.UUID) (Document, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	index, exists := store.byID[id]
	if !exists {
		return Document{}, ErrNotFound
	}
	return cloneDocument(store.documents[index]), nil
}

// ByOrder implements Store.
func (store *MemoryStore) ByOrder(_ context.Context, orderID uuid.UUID) ([]Document, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	invoices := make(map[uuid.UUID]struct{})
	for _, document := range store.documents {
		if document.OrderID != nil && *document.OrderID == orderID {
			invoices[document.ID] = struct{}{}
		}
	}
	result := make([]Document, 0, len(invoices))
	for _, document := range store.documents {
		if document.OrderID != nil && *document.OrderID == orderID {
			result = append(result, cloneDocument(document))
			continue
		}
		if document.OriginalID == nil {
			continue
		}
		if _, linked := invoices[*document.OriginalID]; linked {
			result = append(result, cloneDocument(document))
		}
	}
	sort.SliceStable(result, func(first, second int) bool {
		if result[first].SequenceYear != result[second].SequenceYear {
			return result[first].SequenceYear < result[second].SequenceYear
		}
		return result[first].SequenceNumber < result[second].SequenceNumber
	})
	return result, nil
}

// ByIdempotencyKey implements Store.
func (store *MemoryStore) ByIdempotencyKey(_ context.Context, key uuid.UUID) (Document, error) {
	if key == uuid.Nil {
		return Document{}, ErrNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.byKey[key]; !exists {
		return Document{}, ErrNotFound
	}
	for _, document := range store.documents {
		if document.IdempotencyKey != nil && *document.IdempotencyKey == key {
			return cloneDocument(document), nil
		}
	}
	return Document{}, ErrNotFound
}

// CreditedTotal implements Store.
func (store *MemoryStore) CreditedTotal(_ context.Context, originalID uuid.UUID) (int64, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var total int64
	for _, document := range store.documents {
		if document.Kind == KindCreditNote &&
			document.OriginalID != nil &&
			*document.OriginalID == originalID {
			total += document.TotalMinor
		}
	}
	return total, nil
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

func cloneDocument(document Document) Document {
	cloned := document
	if document.OrderID != nil {
		value := *document.OrderID
		cloned.OrderID = &value
	}
	if document.OriginalID != nil {
		value := *document.OriginalID
		cloned.OriginalID = &value
	}
	if document.IdempotencyKey != nil {
		value := *document.IdempotencyKey
		cloned.IdempotencyKey = &value
	}
	cloned.IdempotencyPayloadHash = append([]byte(nil), document.IdempotencyPayloadHash...)
	cloned.Snapshot.Lines = append([]Line(nil), document.Snapshot.Lines...)
	return cloned
}
