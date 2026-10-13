package invoiceledger

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// InsertTurnover implements TurnoverStore.
func (store *MemoryStore) InsertTurnover(_ context.Context, entry TurnoverEntry) (TurnoverEntry, error) {
	if entry.ReversalOf != nil || entry.AmountMinor <= 0 ||
		strings.TrimSpace(entry.Reason) != "" ||
		entry.OccurredOn == "" || !validDate(entry.OccurredOn) ||
		!validTurnoverClassification(entry.Classification) ||
		!validCountryCode(entry.CountryCode) {
		return TurnoverEntry{}, ErrUnprocessable
	}
	stored := prepareTurnover(entry)
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.insertTurnoverLocked(stored); err != nil {
		return TurnoverEntry{}, err
	}
	return cloneTurnover(stored), nil
}

// InsertTurnoverReversal implements TurnoverStore. The target row and the
// already-reversed check are read under the store lock, so concurrent
// reversals cannot both succeed.
func (store *MemoryStore) InsertTurnoverReversal(
	_ context.Context,
	entry TurnoverEntry,
) (TurnoverEntry, error) {
	if entry.ReversalOf == nil || *entry.ReversalOf == uuid.Nil ||
		strings.TrimSpace(entry.Reason) == "" {
		return TurnoverEntry{}, ErrUnprocessable
	}
	stored := prepareTurnover(entry)
	store.mu.Lock()
	defer store.mu.Unlock()
	index, exists := store.turnoverID[*stored.ReversalOf]
	if !exists {
		return TurnoverEntry{}, ErrNotFound
	}
	target := store.turnover[index]
	if target.ReversalOf != nil || target.AmountMinor <= 0 {
		return TurnoverEntry{}, ErrConflict
	}
	stored.OccurredOn = target.OccurredOn
	stored.AmountMinor = -target.AmountMinor
	stored.CountryCode = target.CountryCode
	stored.Classification = target.Classification
	stored.Note = target.Note
	if err := store.insertTurnoverLocked(stored); err != nil {
		return TurnoverEntry{}, err
	}
	return cloneTurnover(stored), nil
}

// TurnoverByID implements TurnoverStore.
func (store *MemoryStore) TurnoverByID(_ context.Context, id uuid.UUID) (TurnoverEntry, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	index, exists := store.turnoverID[id]
	if !exists {
		return TurnoverEntry{}, ErrNotFound
	}
	return cloneTurnover(store.turnover[index]), nil
}

// TurnoverByIdempotencyKey implements TurnoverStore.
func (store *MemoryStore) TurnoverByIdempotencyKey(_ context.Context, key uuid.UUID) (TurnoverEntry, error) {
	if key == uuid.Nil {
		return TurnoverEntry{}, ErrNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	index, exists := store.turnoverKey[key]
	if !exists {
		return TurnoverEntry{}, ErrNotFound
	}
	return cloneTurnover(store.turnover[index]), nil
}

// TurnoverByYear implements TurnoverStore.
func (store *MemoryStore) TurnoverByYear(_ context.Context, year int) ([]TurnoverEntry, error) {
	if year < 0 || year > maxSequenceYear {
		return nil, ErrUnprocessable
	}
	prefix := ""
	if year > 0 {
		prefix = fmt.Sprintf("%04d-", year)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	result := make([]TurnoverEntry, 0, len(store.turnover))
	for _, entry := range store.turnover {
		if prefix == "" || strings.HasPrefix(entry.OccurredOn, prefix) {
			result = append(result, cloneTurnover(entry))
		}
	}
	sort.SliceStable(result, func(first, second int) bool {
		if !result[first].CreatedAt.Equal(result[second].CreatedAt) {
			return result[first].CreatedAt.After(result[second].CreatedAt)
		}
		return bytes.Compare(result[first].ID[:], result[second].ID[:]) < 0
	})
	return result, nil
}

func (store *MemoryStore) insertTurnoverLocked(stored TurnoverEntry) error {
	if _, exists := store.turnoverID[stored.ID]; exists {
		return ErrConflict
	}
	if stored.IdempotencyKey != nil {
		if _, exists := store.turnoverKey[*stored.IdempotencyKey]; exists {
			return ErrConflict
		}
	}
	if stored.ReversalOf != nil {
		if _, exists := store.turnoverLink[*stored.ReversalOf]; exists {
			return ErrConflict
		}
	}
	store.turnover = append(store.turnover, stored)
	index := len(store.turnover) - 1
	store.turnoverID[stored.ID] = index
	if stored.IdempotencyKey != nil {
		store.turnoverKey[*stored.IdempotencyKey] = index
	}
	if stored.ReversalOf != nil {
		store.turnoverLink[*stored.ReversalOf] = struct{}{}
	}
	return nil
}

func prepareTurnover(entry TurnoverEntry) TurnoverEntry {
	stored := cloneTurnover(entry)
	if stored.ID == uuid.Nil {
		stored.ID = uuid.New()
	}
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	}
	return stored
}

func cloneTurnover(entry TurnoverEntry) TurnoverEntry {
	cloned := entry
	if entry.ReversalOf != nil {
		value := *entry.ReversalOf
		cloned.ReversalOf = &value
	}
	if entry.IdempotencyKey != nil {
		value := *entry.IdempotencyKey
		cloned.IdempotencyKey = &value
	}
	cloned.IdempotencyPayloadHash = append([]byte(nil), entry.IdempotencyPayloadHash...)
	return cloned
}
