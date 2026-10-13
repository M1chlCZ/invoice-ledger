package invoiceledger

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// TurnoverClassification classifies one manual external turnover entry for
// the tax monitoring views.
type TurnoverClassification string

const (
	// TurnoverPending marks turnover that is not classified yet. A pending
	// entry keeps the monitoring view marked as incomplete.
	TurnoverPending TurnoverClassification = "pending"

	// TurnoverDomestic marks Czech domestic turnover.
	TurnoverDomestic TurnoverClassification = "domestic"

	// TurnoverEUTBE marks European Union distance-sales and cross-border
	// turnover.
	TurnoverEUTBE TurnoverClassification = "eu_tbe"

	// TurnoverExcluded marks turnover outside the monitored totals.
	TurnoverExcluded TurnoverClassification = "excluded"
)

// TurnoverEntry is one append-only external turnover row. A reversal is a
// separate entry with a negative amount and ReversalOf set. The ledger never
// updates or deletes entries.
type TurnoverEntry struct {
	ID                     uuid.UUID              `json:"id"`
	OccurredOn             string                 `json:"occurred_on"`
	AmountMinor            int64                  `json:"amount_minor"`
	CountryCode            string                 `json:"country_code,omitempty"`
	Classification         TurnoverClassification `json:"classification"`
	Note                   string                 `json:"note,omitempty"`
	ReversalOf             *uuid.UUID             `json:"reversal_of,omitempty"`
	Reason                 string                 `json:"reason,omitempty"`
	CreatedAt              time.Time              `json:"created_at"`
	IdempotencyKey         *uuid.UUID             `json:"idempotency_key,omitempty"`
	IdempotencyPayloadHash []byte                 `json:"idempotency_payload_hash,omitempty"`
}

// TurnoverRequest appends one external turnover entry.
type TurnoverRequest struct {
	OccurredOn     string
	AmountMinor    int64
	CountryCode    string
	Classification TurnoverClassification
	Note           string
	IdempotencyKey uuid.UUID
}

// TurnoverVoidRequest appends a reversing entry for one existing entry. The
// original row is never changed or deleted.
type TurnoverVoidRequest struct {
	EntryID        uuid.UUID
	Reason         string
	IdempotencyKey uuid.UUID
}

// CreateTurnover appends one external turnover entry for a positive amount.
// CreateTurnover is idempotent by key and payload hash and is safe to replay.
//
// A repeated key with the same payload returns the stored entry with replayed
// true. The same key with a different payload returns ErrIdempotencyConflict.
// The occurrence date is required and must use the 2006-01-02 layout; the
// amount must be positive; the country code is empty or two uppercase letters;
// the classification is one of the TurnoverClassification values. The note is
// optional and limited to 500 runes. The configured Store must implement
// TurnoverStore or CreateTurnover returns ErrUnsupported.
func (service *Service) CreateTurnover(
	ctx context.Context,
	request TurnoverRequest,
) (TurnoverEntry, bool, error) {
	if request.OccurredOn == "" || !validDate(request.OccurredOn) ||
		request.AmountMinor <= 0 ||
		!validTurnoverClassification(request.Classification) ||
		!validCountryCode(request.CountryCode) ||
		!validText(request.Note, reasonMaxRunes, false) {
		return TurnoverEntry{}, false, ErrUnprocessable
	}
	store, ok := service.store.(TurnoverStore)
	if !ok {
		return TurnoverEntry{}, false, ErrUnsupported
	}
	hash := payloadHash(
		"external", request.OccurredOn, strconv.FormatInt(request.AmountMinor, 10),
		request.CountryCode, string(request.Classification), request.Note,
	)
	entry, replayed, err := findTurnover(ctx, store, request.IdempotencyKey, hash)
	if err != nil || replayed {
		return entry, replayed, err
	}
	stored, err := store.InsertTurnover(ctx, TurnoverEntry{
		OccurredOn:             request.OccurredOn,
		AmountMinor:            request.AmountMinor,
		CountryCode:            request.CountryCode,
		Classification:         request.Classification,
		Note:                   request.Note,
		IdempotencyKey:         optionalUUID(request.IdempotencyKey),
		IdempotencyPayloadHash: hash,
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return replayTurnover(ctx, store, request.IdempotencyKey, hash)
		}
		return TurnoverEntry{}, false, err
	}
	return stored, false, nil
}

// VoidTurnover appends a reversing entry for one external turnover entry.
// VoidTurnover is idempotent by key and payload hash and is safe to replay.
//
// The reason is required and limited to 500 runes. A repeated key with the
// same payload returns the stored reversal with replayed true; the same key
// with a different payload returns ErrIdempotencyConflict. A missing entry
// returns ErrNotFound. A target that is itself a reversal, carries a
// non-positive amount, or already has a reversal returns ErrConflict. The
// configured Store must implement TurnoverStore or VoidTurnover returns
// ErrUnsupported.
func (service *Service) VoidTurnover(
	ctx context.Context,
	request TurnoverVoidRequest,
) (TurnoverEntry, bool, error) {
	if !validText(request.Reason, reasonMaxRunes, true) {
		return TurnoverEntry{}, false, ErrUnprocessable
	}
	store, ok := service.store.(TurnoverStore)
	if !ok {
		return TurnoverEntry{}, false, ErrUnsupported
	}
	hash := payloadHash("external_void", request.EntryID.String(), request.Reason)
	entry, replayed, err := findTurnover(ctx, store, request.IdempotencyKey, hash)
	if err != nil || replayed {
		return entry, replayed, err
	}
	target, err := store.TurnoverByID(ctx, request.EntryID)
	if err != nil {
		return TurnoverEntry{}, false, err
	}
	if target.ReversalOf != nil || target.AmountMinor <= 0 {
		return TurnoverEntry{}, false, ErrConflict
	}
	stored, err := store.InsertTurnoverReversal(ctx, TurnoverEntry{
		ReversalOf:             &target.ID,
		Reason:                 request.Reason,
		IdempotencyKey:         optionalUUID(request.IdempotencyKey),
		IdempotencyPayloadHash: hash,
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return replayTurnover(ctx, store, request.IdempotencyKey, hash)
		}
		return TurnoverEntry{}, false, err
	}
	return stored, false, nil
}

// ListTurnover returns the external turnover entries of one calendar year, or
// every entry when year is zero. Entries are ordered by creation time
// descending and then by ID ascending. A year outside 0 to 9999 returns
// ErrUnprocessable. The configured Store must implement TurnoverStore or
// ListTurnover returns ErrUnsupported.
func (service *Service) ListTurnover(ctx context.Context, year int) ([]TurnoverEntry, error) {
	if year < 0 || year > maxSequenceYear {
		return nil, ErrUnprocessable
	}
	store, ok := service.store.(TurnoverStore)
	if !ok {
		return nil, ErrUnsupported
	}
	return store.TurnoverByYear(ctx, year)
}

func findTurnover(
	ctx context.Context,
	store TurnoverStore,
	key uuid.UUID,
	hash []byte,
) (TurnoverEntry, bool, error) {
	if key == uuid.Nil {
		return TurnoverEntry{}, false, nil
	}
	existing, err := store.TurnoverByIdempotencyKey(ctx, key)
	switch {
	case err == nil:
		idempotencyErr := checkIdempotency(
			existing.IdempotencyKey, existing.IdempotencyPayloadHash, key, hash,
		)
		if idempotencyErr != nil {
			return TurnoverEntry{}, false, idempotencyErr
		}
		return existing, true, nil
	case errors.Is(err, ErrNotFound):
		return TurnoverEntry{}, false, nil
	default:
		return TurnoverEntry{}, false, err
	}
}

func replayTurnover(
	ctx context.Context,
	store TurnoverStore,
	key uuid.UUID,
	hash []byte,
) (TurnoverEntry, bool, error) {
	entry, found, err := findTurnover(ctx, store, key, hash)
	if err != nil {
		return TurnoverEntry{}, false, err
	}
	if !found {
		return TurnoverEntry{}, false, ErrConflict
	}
	return entry, true, nil
}

func validTurnoverClassification(value TurnoverClassification) bool {
	switch value {
	case TurnoverPending, TurnoverDomestic, TurnoverEUTBE, TurnoverExcluded:
		return true
	default:
		return false
	}
}

func validCountryCode(value string) bool {
	if value == "" {
		return true
	}
	if len(value) != 2 {
		return false
	}
	for _, symbol := range value {
		if symbol < 'A' || symbol > 'Z' {
			return false
		}
	}
	return true
}
