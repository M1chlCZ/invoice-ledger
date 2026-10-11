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
