// Package storetest provides a conformance suite for invoiceledger.Store
// implementations. Adapter authors run Run against a factory that returns a
// fresh, empty store.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/m1chlcz/invoice-ledger"
)

const (
	testSequenceYear  = 2026
	testNextYear      = 2027
	testYearBeforeMin = 1999
	testYearAfterMax  = 10000
	testWorkers       = 16
	testOrderNumber   = "1001"
	testCurrency      = "CZK"
	testAmountMinor   = 1000
	testCreditMinor   = -500
	testCreditFirst   = -300
	testCreditSecond  = -200
	testCreditLarge   = -600
	testCreditOver    = -500
	testCreditFinal   = -400
)

const (
	testSequenceFirst = iota + 1
	testSequenceSecond
	testSequenceThird
	testSequenceFourth
)

// Run verifies the documented Store contract against a fresh store from the
// factory for each subtest.
func Run(t *testing.T, factory func(t *testing.T) invoiceledger.Store) {
	t.Helper()
	t.Run("allocate number is monotonic per year", func(t *testing.T) {
		checkAllocateNumberMonotonic(t, factory)
	})
	t.Run("concurrent allocations are unique", func(t *testing.T) {
		checkConcurrentAllocationsUnique(t, factory)
	})
	t.Run("insert round trips by id", func(t *testing.T) {
		checkInsertRoundTrip(t, factory)
	})
	t.Run("insert enforces uniqueness", func(t *testing.T) {
		checkInsertUniqueness(t, factory)
	})
	t.Run("idempotency key lookup", func(t *testing.T) {
		checkIdempotencyKeyLookup(t, factory)
	})
	t.Run("order lookup includes linked credit notes", func(t *testing.T) {
		checkOrderLookup(t, factory)
	})
	t.Run("credited total sums linked credits", func(t *testing.T) {
		checkCreditedTotalSums(t, factory)
	})
	t.Run("credited total counts only credit notes", func(t *testing.T) {
		checkCreditedTotalKinds(t, factory)
	})
	t.Run("credit inserter enforces the limit atomically", func(t *testing.T) {
		checkCreditInserterLimit(t, factory)
	})
	t.Run("returned documents are copies", func(t *testing.T) {
		checkReturnedDocumentsAreCopies(t, factory)
	})
}

func checkAllocateNumberMonotonic(t *testing.T, factory func(t *testing.T) invoiceledger.Store) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()
	first, err := store.AllocateNumber(ctx, testSequenceYear, invoiceledger.KindInvoice)
	if err != nil || first.Number != testSequenceFirst || first.Year != testSequenceYear {
		t.Fatalf("first allocation = %+v, %v", first, err)
	}
	second, err := store.AllocateNumber(ctx, testSequenceYear, invoiceledger.KindCreditNote)
	if err != nil || second.Number != testSequenceSecond {
		t.Fatalf("second allocation = %+v, %v", second, err)
	}
	next, err := store.AllocateNumber(ctx, testNextYear, invoiceledger.KindInvoice)
	if err != nil || next.Number != testSequenceFirst {
		t.Fatalf("next year allocation = %+v, %v", next, err)
	}
	if _, earlyErr := store.AllocateNumber(ctx, testYearBeforeMin, invoiceledger.KindInvoice); earlyErr == nil {
		t.Fatal("allocation before the supported years did not fail")
	}
	if _, lateErr := store.AllocateNumber(ctx, testYearAfterMax, invoiceledger.KindInvoice); lateErr == nil {
		t.Fatal("allocation after the supported years did not fail")
	}
}

func checkConcurrentAllocationsUnique(t *testing.T, factory func(t *testing.T) invoiceledger.Store) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()
	numbers := make(chan int64, testWorkers)
	var waitGroup sync.WaitGroup
	for range testWorkers {
		waitGroup.Go(func() {
			sequence, err := store.AllocateNumber(ctx, testSequenceYear, invoiceledger.KindInvoice)
			if err != nil {
				numbers <- 0
				return
			}
			numbers <- sequence.Number
		})
	}
	waitGroup.Wait()
	close(numbers)
	seen := make(map[int64]struct{}, testWorkers)
	for number := range numbers {
		if number == 0 {
			t.Fatal("concurrent allocation failed")
		}
		if _, duplicate := seen[number]; duplicate {
			t.Fatalf("duplicate allocation %d", number)
		}
		seen[number] = struct{}{}
	}
	if len(seen) != testWorkers {
		t.Fatalf("unique allocations = %d, want %d", len(seen), testWorkers)
	}
}

func checkInsertRoundTrip(t *testing.T, factory func(t *testing.T) invoiceledger.Store) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()
	document := testInvoice(uuid.New(), testSequenceFirst, "FA-2026-000001")
	document.ID = uuid.Nil
	document.CreatedAt = time.Time{}
	stored, err := store.Insert(ctx, document)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if stored.ID == uuid.Nil || stored.CreatedAt.IsZero() {
		t.Fatalf("insert did not assign id and created time: %+v", stored)
	}
	got, err := store.ByID(ctx, stored.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Number != stored.Number || got.TotalMinor != stored.TotalMinor ||
		got.Snapshot.Lines[0].Name != stored.Snapshot.Lines[0].Name {
		t.Fatalf("round trip = %+v, want %+v", got, stored)
	}
	if _, readErr := store.ByID(ctx, uuid.New()); !errors.Is(readErr, invoiceledger.ErrNotFound) {
		t.Fatalf("unknown id error = %v", readErr)
	}
}

func checkInsertUniqueness(t *testing.T, factory func(t *testing.T) invoiceledger.Store) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()
	orderID := uuid.New()
	first := testInvoice(orderID, testSequenceFirst, "FA-2026-000001")
	if _, err := store.Insert(ctx, first); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	duplicate := first
	if _, err := store.Insert(ctx, duplicate); !errors.Is(err, invoiceledger.ErrConflict) {
		t.Fatalf("duplicate id error = %v", err)
	}
	sameOrder := testInvoice(orderID, testSequenceSecond, "FA-2026-000002")
	if _, err := store.Insert(ctx, sameOrder); !errors.Is(err, invoiceledger.ErrConflict) {
		t.Fatalf("second invoice for the order error = %v", err)
	}
	sameNumber := testInvoice(uuid.New(), testSequenceSecond, "FA-2026-000001")
	if _, err := store.Insert(ctx, sameNumber); !errors.Is(err, invoiceledger.ErrConflict) {
		t.Fatalf("duplicate number error = %v", err)
	}
	sameSequence := testInvoice(uuid.New(), testSequenceFirst, "FA-2026-000099")
	if _, err := store.Insert(ctx, sameSequence); !errors.Is(err, invoiceledger.ErrConflict) {
		t.Fatalf("duplicate sequence error = %v", err)
	}
	key := uuid.New()
	withKey := testInvoice(uuid.New(), testSequenceThird, "FA-2026-000003")
	withKey.IdempotencyKey = &key
	if _, err := store.Insert(ctx, withKey); err != nil {
		t.Fatalf("insert with key: %v", err)
	}
	sameKey := testInvoice(uuid.New(), testSequenceFourth, "FA-2026-000004")
	sameKey.IdempotencyKey = &key
	if _, err := store.Insert(ctx, sameKey); !errors.Is(err, invoiceledger.ErrConflict) {
		t.Fatalf("duplicate idempotency key error = %v", err)
	}
}

func checkIdempotencyKeyLookup(t *testing.T, factory func(t *testing.T) invoiceledger.Store) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()
	if _, err := store.ByIdempotencyKey(ctx, uuid.Nil); !errors.Is(err, invoiceledger.ErrNotFound) {
		t.Fatalf("nil key error = %v", err)
	}
	if _, err := store.ByIdempotencyKey(ctx, uuid.New()); !errors.Is(err, invoiceledger.ErrNotFound) {
		t.Fatalf("unknown key error = %v", err)
	}
	key := uuid.New()
	document := testInvoice(uuid.New(), testSequenceFirst, "FA-2026-000001")
	document.IdempotencyKey = &key
	document.IdempotencyPayloadHash = []byte("payload")
	if _, err := store.Insert(ctx, document); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := store.ByIdempotencyKey(ctx, key)
	if err != nil || got.ID != document.ID {
		t.Fatalf("key lookup = %+v, %v", got, err)
	}
}

func checkOrderLookup(t *testing.T, factory func(t *testing.T) invoiceledger.Store) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()
	orderID := uuid.New()
	invoice := testInvoice(orderID, testSequenceFirst, "FA-2026-000001")
	if _, err := store.Insert(ctx, invoice); err != nil {
		t.Fatalf("insert invoice: %v", err)
	}
	credit := testCredit(invoice, testSequenceSecond, "OP-2026-000002")
	if _, err := store.Insert(ctx, credit); err != nil {
		t.Fatalf("insert credit: %v", err)
	}
	otherInvoice := testInvoice(uuid.New(), testSequenceThird, "FA-2026-000003")
	if _, err := store.Insert(ctx, otherInvoice); err != nil {
		t.Fatalf("insert other invoice: %v", err)
	}
	otherCredit := testCredit(otherInvoice, testSequenceFourth, "OP-2026-000004")
	if _, err := store.Insert(ctx, otherCredit); err != nil {
		t.Fatalf("insert other credit: %v", err)
	}
	documents, err := store.ByOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("order lookup: %v", err)
	}
	if len(documents) != 2 || documents[0].ID != invoice.ID || documents[1].ID != credit.ID {
		t.Fatalf("order documents = %+v", documents)
	}
	empty, emptyErr := store.ByOrder(ctx, uuid.New())
	if emptyErr != nil || len(empty) != 0 {
		t.Fatalf("unknown order documents = %+v, %v", empty, emptyErr)
	}
}

func checkCreditedTotalSums(t *testing.T, factory func(t *testing.T) invoiceledger.Store) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()
	orderID := uuid.New()
	invoice := testInvoice(orderID, testSequenceFirst, "FA-2026-000001")
	if _, err := store.Insert(ctx, invoice); err != nil {
		t.Fatalf("insert invoice: %v", err)
	}
	total, err := store.CreditedTotal(ctx, invoice.ID)
	if err != nil || total != 0 {
		t.Fatalf("empty credited total = %d, %v", total, err)
	}
	first := testCredit(invoice, testSequenceSecond, "OP-2026-000002")
	first.TotalMinor = testCreditFirst
	if _, insertErr := store.Insert(ctx, first); insertErr != nil {
		t.Fatalf("insert first credit: %v", insertErr)
	}
	second := testCredit(invoice, testSequenceThird, "OP-2026-000003")
	second.TotalMinor = testCreditSecond
	if _, insertErr := store.Insert(ctx, second); insertErr != nil {
		t.Fatalf("insert second credit: %v", insertErr)
	}
	total, err = store.CreditedTotal(ctx, invoice.ID)
	if err != nil || total != testCreditFirst+testCreditSecond {
		t.Fatalf("credited total = %d, %v", total, err)
	}
}

func checkCreditedTotalKinds(t *testing.T, factory func(t *testing.T) invoiceledger.Store) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()
	orderID := uuid.New()
	invoice := testInvoice(orderID, testSequenceFirst, "FA-2026-000001")
	if _, err := store.Insert(ctx, invoice); err != nil {
		t.Fatalf("insert invoice: %v", err)
	}
	stray := testInvoice(uuid.New(), testSequenceSecond, "FA-2026-000002")
	stray.OrderID = nil
	originalID := invoice.ID
	stray.OriginalID = &originalID
	if _, err := store.Insert(ctx, stray); err != nil {
		t.Fatalf("insert stray document: %v", err)
	}
	total, err := store.CreditedTotal(ctx, invoice.ID)
	if err != nil || total != 0 {
		t.Fatalf("credited total with non-credit link = %d, %v", total, err)
	}
}

func checkCreditInserterLimit(t *testing.T, factory func(t *testing.T) invoiceledger.Store) {
	t.Helper()
	store := factory(t)
	inserter, ok := store.(invoiceledger.CreditInserter)
	if !ok {
		t.Skip("store does not implement invoiceledger.CreditInserter")
	}
	ctx := context.Background()
	orderID := uuid.New()
	invoice := testInvoice(orderID, testSequenceFirst, "FA-2026-000001")
	if _, err := store.Insert(ctx, invoice); err != nil {
		t.Fatalf("insert invoice: %v", err)
	}
	first := testCredit(invoice, testSequenceSecond, "OP-2026-000002")
	first.TotalMinor = testCreditLarge
	if _, err := inserter.InsertCredit(ctx, first, invoice.ID, invoice.TotalMinor); err != nil {
		t.Fatalf("first InsertCredit: %v", err)
	}
	over := testCredit(invoice, testSequenceThird, "OP-2026-000003")
	over.TotalMinor = testCreditOver
	if _, err := inserter.InsertCredit(
		ctx,
		over,
		invoice.ID,
		invoice.TotalMinor,
	); !errors.Is(
		err,
		invoiceledger.ErrConflict,
	) {
		t.Fatalf("over-limit InsertCredit error = %v", err)
	}
	total, err := store.CreditedTotal(ctx, invoice.ID)
	if err != nil || total != testCreditLarge {
		t.Fatalf("credited total after rejection = %d, %v", total, err)
	}
	if _, lookupErr := store.ByID(ctx, over.ID); !errors.Is(lookupErr, invoiceledger.ErrNotFound) {
		t.Fatalf("rejected credit was persisted: %v", lookupErr)
	}
	final := testCredit(invoice, testSequenceThird, "OP-2026-000003")
	final.TotalMinor = testCreditFinal
	if _, insertErr := inserter.InsertCredit(ctx, final, invoice.ID, invoice.TotalMinor); insertErr != nil {
		t.Fatalf("final InsertCredit: %v", insertErr)
	}
	total, err = store.CreditedTotal(ctx, invoice.ID)
	if err != nil || total != testCreditLarge+testCreditFinal {
		t.Fatalf("final credited total = %d, %v", total, err)
	}
}

func checkReturnedDocumentsAreCopies(t *testing.T, factory func(t *testing.T) invoiceledger.Store) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()
	document := testInvoice(uuid.New(), testSequenceFirst, "FA-2026-000001")
	document.Snapshot.Lines[0].Name = "Original line"
	stored, err := store.Insert(ctx, document)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	stored.Snapshot.Lines[0].Name = "Mutated line"
	*stored.OrderID = uuid.New()
	got, err := store.ByID(ctx, stored.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Snapshot.Lines[0].Name != "Original line" || *got.OrderID == *stored.OrderID {
		t.Fatalf("store exposed internal state: %+v", got)
	}
}

func testInvoice(orderID uuid.UUID, sequence int64, number string) invoiceledger.Document {
	order := orderID
	return invoiceledger.Document{
		ID:             uuid.New(),
		SequenceYear:   testSequenceYear,
		SequenceNumber: sequence,
		Number:         number,
		Kind:           invoiceledger.KindInvoice,
		OrderID:        &order,
		OrderNumber:    testOrderNumber,
		Currency:       testCurrency,
		ItemsMinor:     testAmountMinor,
		ShippingMinor:  0,
		DiscountMinor:  0,
		TotalMinor:     testAmountMinor,
		IssuedDate:     "2026-01-01",
		Paid:           true,
		Snapshot: invoiceledger.Snapshot{
			Number:      number,
			Kind:        invoiceledger.KindInvoice,
			OrderNumber: testOrderNumber,
			Currency:    testCurrency,
			Lines: []invoiceledger.Line{{
				Name: "Example product", Quantity: 1,
				UnitPriceMinor: testAmountMinor, TotalMinor: testAmountMinor,
			}},
			ItemsMinor: testAmountMinor,
			TotalMinor: testAmountMinor,
			Paid:       true,
		},
	}
}

func testCredit(original invoiceledger.Document, sequence int64, number string) invoiceledger.Document {
	originalID := original.ID
	return invoiceledger.Document{
		ID:              uuid.New(),
		SequenceYear:    testSequenceYear,
		SequenceNumber:  sequence,
		Number:          number,
		Kind:            invoiceledger.KindCreditNote,
		OrderNumber:     original.OrderNumber,
		OriginalID:      &originalID,
		OriginalNumber:  original.Number,
		RefundReference: "REF-1",
		RefundedOn:      "2026-01-02",
		Currency:        testCurrency,
		ItemsMinor:      testCreditMinor,
		ShippingMinor:   0,
		DiscountMinor:   0,
		TotalMinor:      testCreditMinor,
		IssuedDate:      "2026-01-02",
		Paid:            true,
		Snapshot: invoiceledger.Snapshot{
			Number:         number,
			Kind:           invoiceledger.KindCreditNote,
			OriginalNumber: original.Number,
			OrderNumber:    original.OrderNumber,
			Currency:       testCurrency,
			Lines: []invoiceledger.Line{{
				Name: fmt.Sprintf("Credit note for %s", original.Number), Quantity: 1,
				UnitPriceMinor: testCreditMinor, TotalMinor: testCreditMinor,
			}},
			ItemsMinor: testCreditMinor,
			TotalMinor: testCreditMinor,
			Paid:       true,
		},
	}
}
