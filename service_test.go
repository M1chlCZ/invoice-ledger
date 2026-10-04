//nolint:testpackage // White-box tests exercise unexported reconcileItems and reasonMaxRunes plus shared fixtures.
package invoiceledger

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	testSupplyDate = "2026-03-14"
	testRefundDate = "2026-03-16"
)

type memorySource struct {
	orders map[uuid.UUID]OrderFacts
	err    error
}

func (source *memorySource) Order(_ context.Context, orderID uuid.UUID) (OrderFacts, error) {
	if source.err != nil {
		return OrderFacts{}, source.err
	}
	facts, ok := source.orders[orderID]
	if !ok {
		return OrderFacts{}, ErrNotFound
	}
	return facts, nil
}

type mismatchedSource struct {
	facts OrderFacts
}

func (source mismatchedSource) Order(context.Context, uuid.UUID) (OrderFacts, error) {
	return source.facts, nil
}

type fixture struct {
	t       *testing.T
	service *Service
	store   *MemoryStore
	source  *memorySource
	now     *time.Time
}

func (f *fixture) setNow(value time.Time) {
	*f.now = value
}

func testSeller() Seller {
	return Seller{
		Name:    "Example Seller",
		ID:      "00000000",
		Address: "1 Example Street, 110 00 Example City",
		Email:   "seller@example.test",
	}
}

func newFixture(t *testing.T, customize func(*Options), orders ...OrderFacts) *fixture {
	t.Helper()
	source := &memorySource{orders: make(map[uuid.UUID]OrderFacts)}
	for _, order := range orders {
		source.orders[order.OrderID] = order
	}
	store := NewMemoryStore()
	now := time.Date(2026, time.March, 15, 9, 0, 0, 0, time.UTC)
	options := Options{
		Source: source,
		Store:  store,
		Clock:  func() time.Time { return now },
		Seller: testSeller(),
	}
	if customize != nil {
		customize(&options)
	}
	service, err := New(options)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return &fixture{t: t, service: service, store: store, source: source, now: &now}
}

func testOrderFacts(orderID uuid.UUID, orderNumber, items, shipping, discount int64) OrderFacts {
	return OrderFacts{
		OrderID:            orderID,
		OrderNumber:        orderNumber,
		Currency:           "CZK",
		Locale:             "cs-CZ",
		ContactEmail:       "buyer@example.test",
		BillingAddress:     testAddress("CZ"),
		ShippingAddress:    testAddress("CZ"),
		ItemsTotalMinor:    items,
		ShippingTotalMinor: shipping,
		DiscountTotalMinor: discount,
		GrandTotalMinor:    items + shipping - discount,
		Items: []OrderItem{{
			Name:           "Example product",
			Configuration:  "  Example   configuration ",
			Quantity:       1,
			UnitPriceMinor: items,
			LineTotalMinor: items,
		}},
		Paid: true,
	}
}

func testAddress(country string) Address {
	return Address{
		FirstName:   "Test",
		LastName:    "Buyer",
		Street:      "1 Example Street",
		City:        "Example City",
		PostalCode:  "11000",
		CountryCode: country,
	}
}

func (f *fixture) issue(orderID uuid.UUID, key uuid.UUID) Document {
	f.t.Helper()
	document, replayed, err := f.service.Issue(context.Background(), IssueRequest{
		OrderID: orderID, SupplyDate: testSupplyDate, IdempotencyKey: key,
	})
	if err != nil {
		f.t.Fatalf("issue: %v", err)
	}
	if replayed {
		f.t.Fatalf("issue unexpectedly replayed: %+v", document)
	}
	return document
}

func TestNewRequiresStoreAndSource(t *testing.T) {
	t.Parallel()

	if _, err := New(Options{Source: &memorySource{}}); !errors.Is(err, ErrMisconfigured) {
		t.Fatalf("missing store error = %v", err)
	}
	if _, err := New(Options{Store: NewMemoryStore()}); !errors.Is(err, ErrMisconfigured) {
		t.Fatalf("missing source error = %v", err)
	}
	if _, err := New(Options{Source: &memorySource{}}); errors.Is(err, ErrUnavailable) {
		t.Fatalf("misconfiguration reported as unavailable: %v", err)
	}
}

func TestIssueCreatesFrozenDocumentAndReplays(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 100, 0)
	facts.ShippingMethodName = "  Example Carrier  "
	facts.HasShippingMethod = true
	f := newFixture(t, nil, facts)
	ctx := context.Background()
	key := uuid.New()
	document, replayed, err := f.service.Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, SupplyDate: testSupplyDate,
		Reason: "manual issue", IdempotencyKey: key,
	})
	if err != nil || replayed {
		t.Fatalf("issue = %+v replayed=%t err=%v", document, replayed, err)
	}
	assertInvoiceProjection(t, facts, document)
	assertInvoiceParties(t, document)
	assertIssueReplay(t, f, facts, document, key)
}

func assertInvoiceProjection(t *testing.T, facts OrderFacts, document Document) {
	t.Helper()
	if document.Number != "FA-2026-000001" || document.SequenceYear != 2026 || document.SequenceNumber != 1 {
		t.Fatalf("numbering = %+v", document)
	}
	if document.Kind != KindInvoice || document.ItemsMinor != 1000 ||
		document.ShippingMinor != 100 || document.DiscountMinor != 0 ||
		document.TotalMinor != 1100 {
		t.Fatalf("totals = %+v", document)
	}
	if document.OrderID == nil || *document.OrderID != facts.OrderID || document.OrderNumber != "1001" {
		t.Fatalf("order projection = %+v", document)
	}
	if document.SupplyDate != testSupplyDate || document.IssuedDate != "2026-03-15" || !document.Paid {
		t.Fatalf("dates = %+v", document)
	}
	if document.BuyerCountry != "CZ" || document.Locale != "cs-CZ" || document.Currency != "CZK" {
		t.Fatalf("dimensions = %+v", document)
	}
	if len(document.Snapshot.Lines) != 2 {
		t.Fatalf("lines = %+v", document.Snapshot.Lines)
	}
	itemLine := document.Snapshot.Lines[0]
	if itemLine.Name != "Example product" || itemLine.Configuration != "Example configuration" ||
		itemLine.Quantity != 1 || itemLine.UnitPriceMinor != 1000 || itemLine.TotalMinor != 1000 {
		t.Fatalf("item line = %+v", itemLine)
	}
	shippingLine := document.Snapshot.Lines[1]
	if shippingLine.Name != "Example Carrier" || shippingLine.TotalMinor != 100 {
		t.Fatalf("shipping line = %+v", shippingLine)
	}
}

func assertInvoiceParties(t *testing.T, document Document) {
	t.Helper()
	if document.Snapshot.Seller.Name != "Example Seller" ||
		document.Snapshot.Seller.Email != "seller@example.test" {
		t.Fatalf("seller = %+v", document.Snapshot.Seller)
	}
	if document.Snapshot.Buyer.Name != "Test Buyer" ||
		document.Snapshot.Buyer.Address != "1 Example Street, Example City, 11000, CZ" ||
		document.Snapshot.Buyer.Email != "buyer@example.test" {
		t.Fatalf("buyer = %+v", document.Snapshot.Buyer)
	}
}

func assertIssueReplay(t *testing.T, f *fixture, facts OrderFacts, document Document, key uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if document.IdempotencyKey == nil || *document.IdempotencyKey != key || len(document.IdempotencyPayloadHash) == 0 {
		t.Fatalf("idempotency = %+v", document)
	}
	replay, replayed, err := f.service.Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, SupplyDate: testSupplyDate,
		Reason: "manual issue", IdempotencyKey: key,
	})
	if err != nil || !replayed || replay.ID != document.ID {
		t.Fatalf("replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
	_, _, err = f.service.Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, SupplyDate: testSupplyDate,
		Reason: "different payload", IdempotencyKey: key,
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different payload error = %v", err)
	}
	again, replayed, err := f.service.Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, SupplyDate: testSupplyDate, Reason: "manual issue",
		IdempotencyKey: uuid.New(),
	})
	if err != nil || !replayed || again.ID != document.ID {
		t.Fatalf("new key replay = %+v replayed=%t err=%v", again, replayed, err)
	}
	stored, err := f.service.Get(ctx, document.ID)
	if err != nil || stored.Number != document.Number || stored.TotalMinor != document.TotalMinor {
		t.Fatalf("get = %+v, %v", stored, err)
	}
}

func TestIssueNumbersAreMonotonicPerYearAndKind(t *testing.T) {
	t.Parallel()

	first := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	second := testOrderFacts(uuid.New(), 1002, 1000, 0, 0)
	third := testOrderFacts(uuid.New(), 1003, 1000, 0, 0)
	f := newFixture(t, nil, first, second, third)
	ctx := context.Background()
	firstDocument := f.issue(first.OrderID, uuid.New())
	secondDocument := f.issue(second.OrderID, uuid.New())
	if firstDocument.Number != "FA-2026-000001" || secondDocument.Number != "FA-2026-000002" {
		t.Fatalf("invoice numbers = %q, %q", firstDocument.Number, secondDocument.Number)
	}
	credit, _, err := f.service.Credit(ctx, CreditRequest{
		InvoiceID: firstDocument.ID, AmountMinor: 100, Reason: "partial refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	})
	if err != nil || credit.Number != "OP-2026-000003" {
		t.Fatalf("credit number = %+v, %v", credit, err)
	}
	f.setNow(time.Date(2027, time.January, 4, 9, 0, 0, 0, time.UTC))
	nextYear := f.issue(third.OrderID, uuid.New())
	if nextYear.Number != "FA-2027-000001" {
		t.Fatalf("next year number = %q", nextYear.Number)
	}
}

func TestIssueUsesLocationYear(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, func(options *Options) {
		options.Clock = func() time.Time {
			return time.Date(2026, time.December, 31, 23, 30, 0, 0, time.UTC)
		}
	}, facts)
	document, _, err := f.service.Issue(context.Background(), IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: uuid.New(),
	})
	if err != nil || document.Number != "FA-2027-000001" || document.IssuedDate != "2027-01-01" {
		t.Fatalf("location year = %+v, %v", document, err)
	}
}

func TestIssueEligibility(t *testing.T) {
	t.Parallel()

	cancelled := testOrderFacts(uuid.New(), 1002, 1000, 0, 0)
	cancelled.Cancelled = true
	unpaid := testOrderFacts(uuid.New(), 1003, 1000, 0, 0)
	unpaid.Paid = false
	legacyItem := testOrderFacts(uuid.New(), 1004, 1000, 0, 0)
	legacyItem.Items[0].TaxMinor = 1
	legacyShipping := testOrderFacts(uuid.New(), 1005, 1000, 0, 0)
	legacyShipping.ShippingTaxRateBasisPoints = 2100
	paid := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, nil, cancelled, unpaid, legacyItem, legacyShipping, paid)
	ctx := context.Background()
	if _, _, err := f.service.Issue(ctx, IssueRequest{
		OrderID: uuid.New(), IdempotencyKey: uuid.New(),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing order error = %v", err)
	}
	for _, order := range []OrderFacts{cancelled, unpaid} {
		if _, _, err := f.service.Issue(ctx, IssueRequest{
			OrderID: order.OrderID, IdempotencyKey: uuid.New(),
		}); !errors.Is(err, ErrConflict) {
			t.Fatalf("ineligible order error = %v", err)
		}
	}
	for _, order := range []OrderFacts{legacyItem, legacyShipping} {
		if _, _, err := f.service.Issue(ctx, IssueRequest{
			OrderID: order.OrderID, IdempotencyKey: uuid.New(),
		}); !errors.Is(err, ErrLegacyTax) {
			t.Fatalf("legacy order error = %v", err)
		}
	}
	document := f.issue(paid.OrderID, uuid.New())
	facts := f.source.orders[paid.OrderID]
	facts.Cancelled = true
	f.source.orders[paid.OrderID] = facts
	replay, replayed, err := f.service.Issue(ctx, IssueRequest{
		OrderID: paid.OrderID, IdempotencyKey: uuid.New(),
	})
	if err != nil || !replayed || replay.ID != document.ID {
		t.Fatalf("cancelled replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
}

func TestIssueValidatesInput(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, nil, facts)
	ctx := context.Background()
	if _, _, err := f.service.Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, SupplyDate: "2026-13-40", IdempotencyKey: uuid.New(),
	}); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("invalid supply date error = %v", err)
	}
	if _, _, err := f.service.Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, Reason: strings.Repeat("a", reasonMaxRunes+1),
		IdempotencyKey: uuid.New(),
	}); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("oversized reason error = %v", err)
	}
	if _, _, err := f.service.Issue(ctx, IssueRequest{
		OrderID: facts.OrderID, Reason: strings.Repeat("a", reasonMaxRunes),
		IdempotencyKey: uuid.New(),
	}); err != nil {
		t.Fatalf("maximum reason error = %v", err)
	}
}

func TestIssueReconcilesTotals(t *testing.T) {
	t.Parallel()

	broken := testOrderFacts(uuid.New(), 1001, 1000, 100, 0)
	broken.GrandTotalMinor = 999
	negative := testOrderFacts(uuid.New(), 1002, 0, 0, 100)
	negative.GrandTotalMinor = -100
	f := newFixture(t, nil, broken, negative)
	ctx := context.Background()
	for _, order := range []OrderFacts{broken, negative} {
		if _, _, err := f.service.Issue(ctx, IssueRequest{
			OrderID: order.OrderID, IdempotencyKey: uuid.New(),
		}); !errors.Is(err, ErrUnprocessable) {
			t.Fatalf("reconcile error = %v", err)
		}
	}
}

func TestReconcileItems(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		items      []OrderItem
		itemsTotal int64
		want       bool
	}{
		{
			name: "matching lines and sum",
			items: []OrderItem{
				{Quantity: 2, UnitPriceMinor: 500, LineTotalMinor: 1000},
				{Quantity: 3, UnitPriceMinor: 100, LineTotalMinor: 300},
			},
			itemsTotal: 1300,
			want:       true,
		},
		{
			name:       "empty lines with zero total",
			items:      nil,
			itemsTotal: 0,
			want:       true,
		},
		{
			name: "zero quantity line is skipped",
			items: []OrderItem{
				{Quantity: 0, UnitPriceMinor: 1000, LineTotalMinor: 0},
				{Quantity: 1, UnitPriceMinor: 1000, LineTotalMinor: 1000},
			},
			itemsTotal: 1000,
			want:       true,
		},
		{
			name: "line sum overflow",
			items: []OrderItem{
				{Quantity: math.MaxInt32, UnitPriceMinor: 1 << 31, LineTotalMinor: 4611686016279904256},
				{Quantity: math.MaxInt32, UnitPriceMinor: 1 << 31, LineTotalMinor: 4611686016279904256},
				{Quantity: math.MaxInt32, UnitPriceMinor: 1 << 31, LineTotalMinor: 4611686016279904256},
			},
			itemsTotal: 0,
			want:       false,
		},
		{
			name:  "zero quantity requires zero line total",
			items: []OrderItem{{Quantity: 0, UnitPriceMinor: 500, LineTotalMinor: 1}},
			want:  false,
		},
		{
			name:  "wrong line total",
			items: []OrderItem{{Quantity: 2, UnitPriceMinor: 500, LineTotalMinor: 999}},
			want:  false,
		},
		{
			name:  "negative quantity",
			items: []OrderItem{{Quantity: -1, UnitPriceMinor: 500, LineTotalMinor: -500}},
			want:  false,
		},
		{
			name:  "negative unit price",
			items: []OrderItem{{Quantity: 1, UnitPriceMinor: -500, LineTotalMinor: -500}},
			want:  false,
		},
		{
			name:  "line overflow",
			items: []OrderItem{{Quantity: math.MaxInt32, UnitPriceMinor: math.MaxInt64, LineTotalMinor: 0}},
			want:  false,
		},
		{
			name:       "line sum mismatch",
			items:      []OrderItem{{Quantity: 1, UnitPriceMinor: 1000, LineTotalMinor: 1000}},
			itemsTotal: 1001,
			want:       false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := reconcileItems(test.items, test.itemsTotal); got != test.want {
				t.Fatalf("reconcileItems(%+v, %d) = %t, want %t",
					test.items, test.itemsTotal, got, test.want)
			}
		})
	}
}

func TestIssueValidatesLineArithmetic(t *testing.T) {
	t.Parallel()

	brokenLine := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	brokenLine.Items[0].LineTotalMinor = 999
	brokenSum := testOrderFacts(uuid.New(), 1002, 1500, 0, 0)
	brokenSum.Items[0].UnitPriceMinor = 1000
	brokenSum.Items[0].LineTotalMinor = 1000
	negativeQuantity := testOrderFacts(uuid.New(), 1003, 1000, 0, 0)
	negativeQuantity.Items[0].Quantity = -1
	overflow := testOrderFacts(uuid.New(), 1004, 0, 0, 0)
	overflow.Items[0].Quantity = math.MaxInt32
	overflow.Items[0].UnitPriceMinor = math.MaxInt64
	overflow.Items[0].LineTotalMinor = 0
	valid := testOrderFacts(uuid.New(), 1005, 1300, 100, 0)
	valid.Items = []OrderItem{
		{Name: "First", Quantity: 2, UnitPriceMinor: 500, LineTotalMinor: 1000},
		{Name: "Second", Quantity: 3, UnitPriceMinor: 100, LineTotalMinor: 300},
	}
	f := newFixture(t, nil, brokenLine, brokenSum, negativeQuantity, overflow, valid)
	ctx := context.Background()
	for _, order := range []OrderFacts{brokenLine, brokenSum, negativeQuantity, overflow} {
		if _, _, err := f.service.Issue(ctx, IssueRequest{
			OrderID: order.OrderID, IdempotencyKey: uuid.New(),
		}); !errors.Is(err, ErrUnprocessable) {
			t.Fatalf("line validation error for order %d = %v", order.OrderNumber, err)
		}
	}
	document := f.issue(valid.OrderID, uuid.New())
	if len(document.Snapshot.Lines) != 3 || document.Snapshot.Lines[0].TotalMinor != 1000 ||
		document.Snapshot.Lines[1].TotalMinor != 300 || document.TotalMinor != 1400 {
		t.Fatalf("valid multi-line document = %+v", document)
	}
}

func TestIssueRejectsMismatchedSourceOrder(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, func(options *Options) {
		options.Source = mismatchedSource{facts: facts}
	})
	if _, _, err := f.service.Issue(context.Background(), IssueRequest{
		OrderID: uuid.New(), IdempotencyKey: uuid.New(),
	}); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("mismatched source order error = %v", err)
	}
}

func TestIssueShippingAndBuyerProjection(t *testing.T) {
	t.Parallel()

	noShipping := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	fallbackShipping := testOrderFacts(uuid.New(), 1002, 1000, 100, 0)
	company := testOrderFacts(uuid.New(), 1003, 1000, 0, 0)
	company.BillingAddress.Company = "Example Company"
	company.BillingAddress.Street = ""
	company.BillingAddress.City = ""
	company.BillingAddress.PostalCode = ""
	delivery := testOrderFacts(uuid.New(), 1004, 1000, 0, 0)
	delivery.ShippingAddress.CountryCode = "sk"
	f := newFixture(t, nil, noShipping, fallbackShipping, company, delivery)
	first := f.issue(noShipping.OrderID, uuid.New())
	if len(first.Snapshot.Lines) != 1 ||
		first.Snapshot.Lines[0].Configuration != "Example configuration" {
		t.Fatalf("no shipping lines = %+v", first.Snapshot.Lines)
	}
	second := f.issue(fallbackShipping.OrderID, uuid.New())
	if second.Snapshot.Lines[1].Name != "Shipping" {
		t.Fatalf("fallback shipping label = %q", second.Snapshot.Lines[1].Name)
	}
	third := f.issue(company.OrderID, uuid.New())
	if third.Snapshot.Buyer.Name != "Example Company" || third.Snapshot.Buyer.Address != "CZ" {
		t.Fatalf("company buyer = %+v", third.Snapshot.Buyer)
	}
	fourth := f.issue(delivery.OrderID, uuid.New())
	if fourth.BuyerCountry != "SK" || !strings.HasSuffix(fourth.Snapshot.Buyer.Address, "CZ") {
		t.Fatalf("delivery projection = %+v", fourth)
	}
}

func TestIssueCustomLabelsAndPrefixes(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 100, 0)
	f := newFixture(t, func(options *Options) {
		options.InvoicePrefix = "INV"
		options.CreditPrefix = "CN"
		options.ShippingLabel = "Postage"
		options.CreditNoteLabel = "Correction of "
	}, facts)
	ctx := context.Background()
	document := f.issue(facts.OrderID, uuid.New())
	if document.Number != "INV-2026-000001" || document.Snapshot.Lines[1].Name != "Postage" {
		t.Fatalf("custom invoice = %+v", document)
	}
	credit, _, err := f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 500, Reason: "partial refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	})
	if err != nil || credit.Number != "CN-2026-000002" {
		t.Fatalf("custom credit = %+v, %v", credit, err)
	}
	line := credit.Snapshot.Lines[0]
	if line.Name != "Correction of INV-2026-000001" || line.UnitPriceMinor != -500 {
		t.Fatalf("custom credit line = %+v", line)
	}
}

func TestIssueSnapshotStaysFrozen(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 100, 0)
	f := newFixture(t, nil, facts)
	ctx := context.Background()
	document := f.issue(facts.OrderID, uuid.New())
	mutated := f.source.orders[facts.OrderID]
	mutated.Items[0].Name = "Changed product"
	mutated.ItemsTotalMinor = 5000
	mutated.GrandTotalMinor = 5100
	f.source.orders[facts.OrderID] = mutated
	stored, err := f.service.Get(ctx, document.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Snapshot.Lines[0].Name != "Example product" || stored.TotalMinor != 1100 {
		t.Fatalf("snapshot changed: %+v", stored)
	}
	stored.Snapshot.Lines[0].Name = "Mutated copy"
	*stored.OrderID = uuid.New()
	again, err := f.service.Get(ctx, document.ID)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if again.Snapshot.Lines[0].Name != "Example product" || *again.OrderID != facts.OrderID {
		t.Fatalf("store exposed internal state: %+v", again)
	}
}

func TestCreditNegatesAndLinksOriginal(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 100, 0)
	f := newFixture(t, nil, facts)
	ctx := context.Background()
	document := f.issue(facts.OrderID, uuid.New())
	key := uuid.New()
	credit, replayed, err := f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 700, Reason: "partial refund",
		RefundReference: "REF-100", RefundedOn: testRefundDate, IdempotencyKey: key,
	})
	if err != nil || replayed {
		t.Fatalf("credit = %+v replayed=%t err=%v", credit, replayed, err)
	}
	assertCreditProjection(t, document, credit)
	assertCreditReplayAndLimits(t, f, document, credit, key)
}

func assertCreditProjection(t *testing.T, document, credit Document) {
	t.Helper()
	if credit.Number != "OP-2026-000002" || credit.Kind != KindCreditNote ||
		credit.TotalMinor != -700 || credit.ItemsMinor != -700 ||
		credit.ShippingMinor != 0 || credit.DiscountMinor != 0 {
		t.Fatalf("credit totals = %+v", credit)
	}
	if credit.OriginalID == nil || *credit.OriginalID != document.ID ||
		credit.OriginalNumber != document.Number || credit.OrderID != nil {
		t.Fatalf("credit links = %+v", credit)
	}
	if credit.RefundReference != "REF-100" || credit.RefundedOn != testRefundDate ||
		credit.Reason != "partial refund" || credit.SupplyDate != testRefundDate {
		t.Fatalf("refund fields = %+v", credit)
	}
	if credit.Snapshot.OriginalNumber != document.Number || credit.Snapshot.Kind != KindCreditNote ||
		credit.Snapshot.Paid != document.Paid {
		t.Fatalf("credit snapshot = %+v", credit.Snapshot)
	}
	line := credit.Snapshot.Lines[0]
	if line.Name != "Credit note for FA-2026-000001" || line.Configuration != "partial refund" ||
		line.Quantity != 1 || line.UnitPriceMinor != -700 || line.TotalMinor != -700 {
		t.Fatalf("credit line = %+v", line)
	}
	if credit.Snapshot.Buyer.Name != document.Snapshot.Buyer.Name ||
		credit.Snapshot.Seller.Name != document.Snapshot.Seller.Name ||
		credit.BuyerCountry != document.BuyerCountry {
		t.Fatalf("credit parties = %+v", credit.Snapshot)
	}
}

func assertCreditReplayAndLimits(t *testing.T, f *fixture, document, credit Document, key uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	credited, err := f.store.CreditedTotal(ctx, document.ID)
	if err != nil || credited != -700 {
		t.Fatalf("credited total = %d, %v", credited, err)
	}
	replay, replayed, err := f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 700, Reason: "partial refund",
		RefundReference: "REF-100", RefundedOn: testRefundDate, IdempotencyKey: key,
	})
	if err != nil || !replayed || replay.ID != credit.ID {
		t.Fatalf("credit replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
	_, _, err = f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 600, Reason: "partial refund",
		RefundReference: "REF-100", RefundedOn: testRefundDate, IdempotencyKey: key,
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("credit payload conflict = %v", err)
	}
	if _, _, creditErr := f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 500, Reason: "too much",
		RefundReference: "REF-101", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	}); !errors.Is(creditErr, ErrConflict) {
		t.Fatalf("over credit error = %v", creditErr)
	}
	remaining, _, err := f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 400, Reason: "final refund",
		RefundReference: "REF-102", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	})
	if err != nil || remaining.Number != "OP-2026-000003" {
		t.Fatalf("remaining credit = %+v, %v", remaining, err)
	}
	if _, _, creditErr := f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 1, Reason: "one more",
		RefundReference: "REF-103", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	}); !errors.Is(creditErr, ErrConflict) {
		t.Fatalf("exhausted credit error = %v", creditErr)
	}
}

func TestCreditValidation(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, nil, facts)
	ctx := context.Background()
	document := f.issue(facts.OrderID, uuid.New())
	if _, _, err := f.service.Credit(ctx, CreditRequest{
		InvoiceID: uuid.New(), AmountMinor: 100, Reason: "refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing original error = %v", err)
	}
	for _, request := range []CreditRequest{
		{InvoiceID: document.ID, AmountMinor: 0, Reason: "refund", RefundReference: "REF-1", RefundedOn: testRefundDate},
		{InvoiceID: document.ID, AmountMinor: -100, Reason: "refund", RefundReference: "REF-1", RefundedOn: testRefundDate},
		{InvoiceID: document.ID, AmountMinor: 100, Reason: "", RefundReference: "REF-1", RefundedOn: testRefundDate},
		{InvoiceID: document.ID, AmountMinor: 100, Reason: "refund", RefundReference: "", RefundedOn: testRefundDate},
		{InvoiceID: document.ID, AmountMinor: 100, Reason: "refund", RefundReference: "REF-1", RefundedOn: ""},
		{InvoiceID: document.ID, AmountMinor: 100, Reason: "refund", RefundReference: "REF-1", RefundedOn: "2026-02-30"},
	} {
		if _, _, err := f.service.Credit(ctx, request); !errors.Is(err, ErrUnprocessable) {
			t.Fatalf("credit validation error = %v", err)
		}
	}
	credit, _, err := f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 100, Reason: "refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	})
	if err != nil {
		t.Fatalf("first credit: %v", err)
	}
	if _, _, creditErr := f.service.Credit(ctx, CreditRequest{
		InvoiceID: credit.ID, AmountMinor: 50, Reason: "refund",
		RefundReference: "REF-2", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	}); !errors.Is(creditErr, ErrConflict) {
		t.Fatalf("credit of a credit error = %v", creditErr)
	}
}

func TestListByOrder(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	other := testOrderFacts(uuid.New(), 1002, 2000, 0, 0)
	f := newFixture(t, nil, facts, other)
	ctx := context.Background()
	document := f.issue(facts.OrderID, uuid.New())
	second, _, err := f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 300, Reason: "first refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	})
	if err != nil {
		t.Fatalf("first credit: %v", err)
	}
	third, _, err := f.service.Credit(ctx, CreditRequest{
		InvoiceID: document.ID, AmountMinor: 200, Reason: "second refund",
		RefundReference: "REF-2", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	})
	if err != nil {
		t.Fatalf("second credit: %v", err)
	}
	f.issue(other.OrderID, uuid.New())
	documents, err := f.service.ListByOrder(ctx, facts.OrderID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(documents) != 3 || documents[0].ID != document.ID ||
		documents[1].ID != second.ID || documents[2].ID != third.ID {
		t.Fatalf("list = %+v", documents)
	}
	empty, err := f.service.ListByOrder(ctx, uuid.New())
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty list = %+v, %v", empty, err)
	}
	if _, fetchErr := f.service.Get(ctx, uuid.New()); !errors.Is(fetchErr, ErrNotFound) {
		t.Fatalf("missing get error = %v", fetchErr)
	}
}

type stubRenderer struct {
	err error
}

func (renderer stubRenderer) Render(_ context.Context, document Document) ([]byte, error) {
	if renderer.err != nil {
		return nil, renderer.err
	}
	return []byte("payload:" + document.Number), nil
}

func TestRender(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, nil, facts)
	ctx := context.Background()
	if _, err := f.service.Render(ctx, uuid.New()); !errors.Is(err, ErrNoRenderer) {
		t.Fatalf("missing renderer error = %v", err)
	}
	rendering := newFixture(t, func(options *Options) {
		options.Renderer = stubRenderer{}
	}, facts)
	document := rendering.issue(facts.OrderID, uuid.New())
	payload, err := rendering.service.Render(ctx, document.ID)
	if err != nil || string(payload) != "payload:"+document.Number {
		t.Fatalf("render = %q, %v", payload, err)
	}
	if _, renderErr := rendering.service.Render(ctx, uuid.New()); !errors.Is(renderErr, ErrNotFound) {
		t.Fatalf("render missing error = %v", renderErr)
	}
	failing := newFixture(t, func(options *Options) {
		options.Renderer = stubRenderer{err: errors.New("renderer down")}
	}, facts)
	failingDocument := failing.issue(facts.OrderID, uuid.New())
	if _, renderErr := failing.service.Render(ctx, failingDocument.ID); renderErr == nil {
		t.Fatal("renderer failure was not reported")
	}
}

func TestIssuePropagatesSourceFailure(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, nil, facts)
	sourceErr := errors.New("source down")
	f.source.err = sourceErr
	if _, _, err := f.service.Issue(context.Background(), IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: uuid.New(),
	}); !errors.Is(err, sourceErr) {
		t.Fatalf("source failure = %v", err)
	}
}

func TestConcurrentIssueCreatesOneInvoice(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, nil, facts)
	const workers = 8
	type outcome struct {
		document Document
		err      error
	}
	results := make(chan outcome, workers)
	var waitGroup sync.WaitGroup
	for range workers {
		waitGroup.Go(func() {
			document, _, err := f.service.Issue(context.Background(), IssueRequest{
				OrderID: facts.OrderID, IdempotencyKey: uuid.New(),
			})
			results <- outcome{document: document, err: err}
		})
	}
	waitGroup.Wait()
	close(results)
	var firstID uuid.UUID
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent issue error = %v", result.err)
		}
		if firstID == uuid.Nil {
			firstID = result.document.ID
		}
		if result.document.ID != firstID {
			t.Fatalf("concurrent issue document = %s, want %s", result.document.ID, firstID)
		}
	}
	documents, err := f.service.ListByOrder(context.Background(), facts.OrderID)
	if err != nil || len(documents) != 1 {
		t.Fatalf("concurrent issue count = %d, %v", len(documents), err)
	}
}

func TestConcurrentIssueSameIdempotencyKeyReplaysOnce(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, nil, facts)
	key := uuid.New()
	const workers = 2
	type outcome struct {
		document Document
		replayed bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan outcome, workers)
	var waitGroup sync.WaitGroup
	for range workers {
		waitGroup.Go(func() {
			<-start
			document, replayed, err := f.service.Issue(context.Background(), IssueRequest{
				OrderID: facts.OrderID, SupplyDate: testSupplyDate, IdempotencyKey: key,
			})
			results <- outcome{document: document, replayed: replayed, err: err}
		})
	}
	close(start)
	waitGroup.Wait()
	close(results)
	var firstID uuid.UUID
	replays := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent same-key issue error = %v", result.err)
		}
		if firstID == uuid.Nil {
			firstID = result.document.ID
		}
		if result.document.ID != firstID {
			t.Fatalf("concurrent same-key document = %s, want %s", result.document.ID, firstID)
		}
		if result.replayed {
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("replays = %d, want exactly 1", replays)
	}
	documents, err := f.service.ListByOrder(context.Background(), facts.OrderID)
	if err != nil || len(documents) != 1 {
		t.Fatalf("concurrent same-key issue count = %d, %v", len(documents), err)
	}
}

func TestConcurrentCreditCannotOverCredit(t *testing.T) {
	t.Parallel()

	facts := testOrderFacts(uuid.New(), 1001, 1000, 0, 0)
	f := newFixture(t, nil, facts)
	document := f.issue(facts.OrderID, uuid.New())
	const workers = 8
	results := make(chan error, workers)
	var waitGroup sync.WaitGroup
	for index := range workers {
		waitGroup.Go(func() {
			_, _, err := f.service.Credit(context.Background(), CreditRequest{
				InvoiceID: document.ID, AmountMinor: 300,
				Reason:          fmt.Sprintf("refund %d", index),
				RefundReference: fmt.Sprintf("REF-%d", index),
				RefundedOn:      testRefundDate, IdempotencyKey: uuid.New(),
			})
			results <- err
		})
	}
	waitGroup.Wait()
	close(results)
	accepted := 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrConflict):
		default:
			t.Fatalf("concurrent credit error = %v", err)
		}
	}
	if accepted != 3 {
		t.Fatalf("accepted credits = %d, want 3", accepted)
	}
	credited, err := f.store.CreditedTotal(context.Background(), document.ID)
	if err != nil || credited != -900 {
		t.Fatalf("credited total = %d, %v", credited, err)
	}
}
