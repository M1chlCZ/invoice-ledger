package invoiceledger_test

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/m1chlcz/invoice-ledger"
)

type exampleSource struct {
	facts invoiceledger.OrderFacts
}

func (source exampleSource) Order(_ context.Context, orderID uuid.UUID) (invoiceledger.OrderFacts, error) {
	if orderID != source.facts.OrderID {
		return invoiceledger.OrderFacts{}, invoiceledger.ErrNotFound
	}
	return source.facts, nil
}

// Example mirrors the README quick start with a fixed clock so the allocated
// numbers are deterministic.
func Example() {
	ctx := context.Background()
	address := invoiceledger.Address{
		FirstName: "Example", LastName: "Buyer",
		Street: "1 Example Street", City: "Example City",
		PostalCode: "11000", CountryCode: "CZ",
	}
	facts := invoiceledger.OrderFacts{
		OrderID:     uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8"),
		OrderNumber: 1001,
		Currency:    "CZK", Locale: "en",
		ContactEmail:    "buyer@example.com",
		BillingAddress:  address,
		ShippingAddress: address,
		ItemsTotalMinor: 1000, ShippingTotalMinor: 100, DiscountTotalMinor: 0,
		GrandTotalMinor: 1100,
		Items: []invoiceledger.OrderItem{{
			Name: "Example product", Quantity: 1,
			UnitPriceMinor: 1000, LineTotalMinor: 1000,
		}},
		Paid: true,
	}
	service, err := invoiceledger.New(invoiceledger.Options{
		Source: exampleSource{facts: facts},
		Store:  invoiceledger.NewMemoryStore(),
		Clock:  func() time.Time { return time.Date(2026, time.March, 15, 9, 0, 0, 0, time.UTC) },
		Seller: invoiceledger.Seller{
			Name: "Example Seller", Address: "1 Example Street",
			Email: "seller@example.com",
		},
	})
	if err != nil {
		panic(err)
	}

	invoice, replayed, err := service.Issue(ctx, invoiceledger.IssueRequest{
		OrderID: facts.OrderID, IdempotencyKey: uuid.New(),
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(invoice.Number, replayed)

	credit, _, err := service.Credit(ctx, invoiceledger.CreditRequest{
		InvoiceID:       invoice.ID,
		AmountMinor:     500,
		Reason:          "partial refund",
		RefundReference: "BANK-1",
		RefundedOn:      "2026-03-16",
		IdempotencyKey:  uuid.New(),
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(credit.Number, credit.TotalMinor)

	// Output:
	// FA-2026-000001 false
	// OP-2026-000002 -500
}
