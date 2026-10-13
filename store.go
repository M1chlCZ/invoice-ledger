package invoiceledger

import (
	"context"

	"github.com/google/uuid"
)

// Store persists the append-only ledger. Implementations must be safe for
// concurrent use and must never expose their internal slices or pointers.
//
// AllocateNumber returns the next number for a calendar year. Numbers start
// at one and increase by one. kind is reserved for per-kind series; the
// bundled stores share one series per year across both kinds. A number that
// is allocated and not inserted stays consumed, so gaps are possible. A year
// outside 2000 to 9999 returns ErrUnprocessable.
//
// Insert appends one document, assigns an ID when Document.ID is zero, and
// assigns CreatedAt when it is zero. Insert returns ErrConflict for a
// duplicate ID, number, year and sequence pair, non-nil idempotency key, or a
// second invoice for the same order.
//
// ByID returns ErrNotFound for an unknown document. ByOrder returns the
// invoice and its linked credit notes for one order, in sequence order; it
// returns an empty list for an unknown order. ByIdempotencyKey returns
// ErrNotFound for a nil or unknown key.
// CreditedTotal returns the sum of the totals of all credit notes linked to
// one original document, or zero when none exist. A linked document of any
// other kind does not count.
//
// Over-credit safety inside one process comes from the Service, which
// serializes Credit calls. Cross-process safety requires the Store to enforce
// the limit atomically: implement CreditInserter, or add an equivalent
// database constraint that Insert respects.
type Store interface {
	AllocateNumber(ctx context.Context, year int, kind Kind) (Sequence, error)
	Insert(ctx context.Context, document Document) (Document, error)
	ByID(ctx context.Context, id uuid.UUID) (Document, error)
	ByOrder(ctx context.Context, orderID uuid.UUID) ([]Document, error)
	ByIdempotencyKey(ctx context.Context, key uuid.UUID) (Document, error)
	CreditedTotal(ctx context.Context, originalID uuid.UUID) (int64, error)
}

// TurnoverStore is an optional Store capability that persists the
// append-only external turnover ledger. Entries are manual turnover rows
// outside the document ledger; reversals are separate rows with a negative
// amount and a link to the row they reverse.
//
// InsertTurnover appends one non-reversal entry, assigns an ID when
// TurnoverEntry.ID is zero, and assigns CreatedAt when it is zero. It returns
// ErrUnprocessable for an entry that carries reversal fields, a non-positive
// amount, a reason, a missing or invalid occurrence date, an unknown
// classification, or an invalid country code, and ErrConflict for a duplicate
// ID or a duplicate non-nil idempotency key.
//
// InsertTurnoverReversal appends the reversal of the entry named by
// TurnoverEntry.ReversalOf. The store locks the target row, accepts only a
// target that is not itself a reversal, carries a positive amount, and has no
// reversal yet, and derives the signed amount and the occurrence date, the
// country code, the classification, and the note from that target.
// TurnoverEntry.Reason is required and is stored on the new row.
// InsertTurnoverReversal assigns an ID when TurnoverEntry.ID is zero and
// CreatedAt when it is zero. It returns ErrNotFound for a missing target,
// ErrUnprocessable for a missing target reference or a missing reason, and
// ErrConflict when the target is ineligible, already reversed, or the new row
// duplicates an ID or a non-nil idempotency key.
//
// TurnoverByID returns one entry or ErrNotFound. TurnoverByIdempotencyKey
// returns one entry or ErrNotFound for a nil or unknown key. TurnoverByYear
// returns the entries whose occurrence date falls in the given calendar year,
// or every entry when year is zero, ordered by creation time descending and
// then by ID ascending; a year outside 0 to 9999 returns ErrUnprocessable.
type TurnoverStore interface {
	InsertTurnover(ctx context.Context, entry TurnoverEntry) (TurnoverEntry, error)
	InsertTurnoverReversal(ctx context.Context, entry TurnoverEntry) (TurnoverEntry, error)
	TurnoverByID(ctx context.Context, id uuid.UUID) (TurnoverEntry, error)
	TurnoverByIdempotencyKey(ctx context.Context, key uuid.UUID) (TurnoverEntry, error)
	TurnoverByYear(ctx context.Context, year int) ([]TurnoverEntry, error)
}

// SettingsStore is an optional Store capability that persists the invoicing
// settings singleton and its durable update request log.
//
// GetSettings returns the current singleton. UpdateSettings replays or
// applies one version-checked update: when the non-nil idempotency key
// already recorded an accepted update it returns that recorded outcome with
// replayed true, and the same key with a different payload hash returns
// ErrIdempotencyConflict. Otherwise the update applies only when the stored
// version equals SettingsUpdate.Version; a stale version returns ErrConflict.
// An accepted update increments the version, stores the given payload hash
// and the applied outcome for a non-nil key, and returns the new settings
// with replayed false.
type SettingsStore interface {
	GetSettings(ctx context.Context) (Settings, error)
	UpdateSettings(
		ctx context.Context,
		update SettingsUpdate,
		payloadHash []byte,
	) (Settings, bool, error)
}

// DocumentLister is an optional Store capability for reading the whole
// append-only ledger. ListDocuments returns every document ordered by
// sequence year and sequence number; an empty ledger returns an empty slice.
type DocumentLister interface {
	ListDocuments(ctx context.Context) ([]Document, error)
}

// CreditInserter is an optional Store capability. A Store that implements it
// persists a credit note only when the sum of the linked credit-note totals
// stays within the given limit, atomically with respect to every concurrent
// writer, including other processes that share the store.
//
// InsertCredit appends one credit note linked to originalID. maxTotalMinor is
// the largest permitted credited total magnitude and is normally the positive
// total of the original invoice: the insert succeeds only when
// CreditedTotal(originalID)+document.TotalMinor is greater than or equal to
// -maxTotalMinor. InsertCredit returns ErrConflict without persisting when
// that condition fails or on any conflict Insert reports, and ErrUnprocessable
// for a document that is not a credit note or for a negative maxTotalMinor.
// Otherwise it behaves like Insert: it assigns ID and CreatedAt when they are
// zero, keeps the same uniqueness rules, and returns a copy.
type CreditInserter interface {
	InsertCredit(ctx context.Context, document Document, originalID uuid.UUID, maxTotalMinor int64) (Document, error)
}
