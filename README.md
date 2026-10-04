# invoice-ledger

`invoice-ledger` is a pure-domain, append-only invoice ledger for Go. It has no
database, HTTP, or rendering code. The caller supplies order facts and storage.

## What it is

- Issues invoices and credit notes as immutable documents.
- Allocates year-bound sequence numbers. Default prefixes are `FA` and `OP`.
- Freezes the snapshot of lines, shipping, discount, totals, and parties at
  issuance time.
- Supports idempotent issuance and credit notes by idempotency key and payload
  hash. A replay returns the stored document.
- Keeps one invoice per order. A credit note always links to its original
  invoice.
- Blocks over-credit above the uncredited remainder.
- Reads one document by ID and lists all documents of one order.

## Why

Invoice logic is easy to get wrong when it is mixed with HTTP handlers,
database rows, and PDF code. This module isolates the rules:

- Integer minor units avoid floating-point rounding surprises.
- Frozen snapshots do not change when a catalog or an order changes later.
- Replay-safe mutations allow safe retries.
- The module stores no personal data by itself. The caller supplies all data
  and controls persistence.

The package imports `time/tzdata`, so the built-in `Europe/Prague` default and
year boundaries keep working on systems without a system time-zone database.
This embeds about 450 KB in any binary that imports the module; set
`Options.Location` to avoid relying on the default.

The module is pre-v1. Its exported API may change between minor releases until
version 1.0.

## Install

```sh
go get github.com/m1chlcz/invoice-ledger
```

## Quick start

Implement a `Source`, create a `Service` with the in-memory store, then issue
an invoice and a credit note.

```go
package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/m1chlcz/invoice-ledger"
)

type source struct {
	facts invoiceledger.OrderFacts
}

func (s source) Order(_ context.Context, orderID uuid.UUID) (invoiceledger.OrderFacts, error) {
	if orderID != s.facts.OrderID {
		return invoiceledger.OrderFacts{}, invoiceledger.ErrNotFound
	}
	return s.facts, nil
}

func main() {
	ctx := context.Background()
	orderID := uuid.New()
	address := invoiceledger.Address{
		FirstName: "Example", LastName: "Buyer",
		Street: "1 Example Street", City: "Example City",
		PostalCode: "11000", CountryCode: "CZ",
	}
	facts := invoiceledger.OrderFacts{
		OrderID: orderID, OrderNumber: 1001,
		Currency: "CZK", Locale: "en",
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
		Source: source{facts: facts},
		Store:  invoiceledger.NewMemoryStore(),
		Seller: invoiceledger.Seller{
			Name: "Example Seller", Address: "1 Example Street",
			Email: "seller@example.com",
		},
	})
	if err != nil {
		panic(err)
	}

	invoice, replayed, err := service.Issue(ctx, invoiceledger.IssueRequest{
		OrderID: orderID, IdempotencyKey: uuid.New(),
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
}
```

## Contracts

### Store

`Store` persists the append-only ledger. All methods are safe for concurrent
use.

| Method | Contract |
| --- | --- |
| `AllocateNumber(ctx, year, kind)` | Returns the next number for the year. Numbers start at one and increase by one. Both kinds share one series per year. `kind` is reserved for per-kind series; the bundled stores share one series per year. |
| `Insert(ctx, document)` | Appends one document. Assigns `ID` and `CreatedAt` when they are empty. Reports `ErrConflict` for a duplicate ID, number, sequence, idempotency key, or a second invoice for one order. |
| `ByID(ctx, id)` | Returns one document or `ErrNotFound`. |
| `ByOrder(ctx, orderID)` | Returns the invoice and its linked credit notes ordered by sequence, or an empty list. |
| `ByIdempotencyKey(ctx, key)` | Returns one document or `ErrNotFound`. |
| `CreditedTotal(ctx, originalID)` | Returns the sum of the linked credit-note totals, or zero. Linked documents of other kinds do not count. |

Numbering gaps are possible by design. `AllocateNumber` consumes a number
before `Insert` runs, and a failed or rejected insert does not give the number
back. That keeps allocation cheap and replay-safe across processes. An adapter
that must not produce gaps has to allocate the number and insert the document
in one transaction, or implement the series inside the database so both run
in one commit.

Over-credit safety inside one process comes from the `Service`, which
serializes `Credit` calls. A shared cross-process store needs atomic
enforcement. A `Store` may implement the optional `CreditInserter` capability:

| Method | Contract |
| --- | --- |
| `InsertCredit(ctx, document, originalID, maxTotalMinor)` | Atomically appends one credit note only when the linked credits stay within `maxTotalMinor`, the positive total of the original invoice. Returns `ErrConflict` without persisting otherwise. |

When the store implements `CreditInserter`, the service uses it for the
insert, so the store enforces the limit atomically. Otherwise the service uses
a check-then-insert, and two processes sharing the store can over-credit
unless `Insert` enforces an equivalent database constraint.

Adapter authors run the conformance suite, which also checks `CreditInserter`
and concurrent allocation when the store provides them:

```go
func TestStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) invoiceledger.Store {
		return myStore(t)
	})
}
```

### Source

`Source.Order` returns the order facts for one order ID. It returns
`ErrNotFound` when the order does not exist. Any other error is propagated to
the caller unchanged. The ledger never reads a database. It reads the order
only through this interface.

`New` returns `ErrMisconfigured` when `Options.Store` or `Options.Source` is
nil.

### Renderer

`Renderer` is an optional hook. Set `Options.Renderer` to turn a stored
document into bytes. Call `Service.Render` to use it. The module does not
import a PDF package. Consumers can plug an external renderer.

A plugged-in `Renderer` is inside the trust boundary for buyer personal data:
it receives the frozen buyer party and order snapshot. Configure only
renderers that your privacy policy allows to process that data.

## Invariants

| Invariant | Behavior |
| --- | --- |
| One invoice per order | The store rejects a second invoice with `ErrConflict`. `Issue` replays the stored invoice. |
| Year-bound numbering | The sequence year comes from `Options.Clock` in `Options.Location`. A new year restarts at one. |
| Possible gaps | An allocated number stays consumed when insertion fails. Gaps are legal by design; gap-free numbering needs a transactional adapter. |
| Line arithmetic | `Quantity * UnitPriceMinor == LineTotalMinor` per line, and line totals sum to `ItemsMinor`, or `ErrUnprocessable`. |
| Reconcile | `ItemsMinor + ShippingMinor - DiscountMinor == TotalMinor`, or `ErrUnprocessable`. |
| Sign rules | An invoice total is zero or positive. A credit-note total is zero or negative. |
| Credit link | A credit note requires an existing original invoice and a positive amount. |
| Over-credit | The sum of credits cannot exceed the original total. |
| Eligible order | A cancelled or unpaid order returns `ErrConflict`. An order with tax data returns `ErrLegacyTax`. |
| Idempotency | A replay of key and payload returns the stored document. A reused key with a different payload returns `ErrIdempotencyConflict`. |
| Immutable documents | The store has no update or delete method. Snapshots are frozen copies. |

## vatkit

`vatkit` is the integer math subpackage.

| Function | Behavior |
| --- | --- |
| `BasisPoints` | A rate from zero to 10000. `2100` means 21.00 percent. |
| `TaxFromGross(gross, rate)` | Returns the tax part of a gross amount. Gross-inclusive, half-up, and overflow-safe. |
| `ApplyBasisPoints(amount, rate)` | Returns the amount plus a half-up rate delta. |
| `IsEUCountry(code)` | Reports European Union member states. Codes are uppercase ISO 3166-1 alpha-2 and match exactly. |
| `EUCountryCodes()` | Returns a sorted copy of the EU member-state codes. |
| `LineTotal(unitPrice, quantity)` | Returns a checked line total. |
| `ShippingTotal(unitPrice, count)` | Returns a checked shipping total for any non-negative shipment count. |
| `Accumulate(current, delta)` | Adds non-negative minor units with an overflow check. |

The module ships opinionated defaults: the `FA` and `OP` number prefixes and
the `Europe/Prague` numbering location. All of them are configurable through
`Options`.

## Data handling

- The module stores no data. The chosen `Store` adapter controls persistence.
- The module makes no network calls and collects no telemetry.
- `Seller`, `Party`, and `OrderFacts` values come from the caller. The module
  copies them into a snapshot and writes no logs.
- The caller is responsible for personal-data policy, encryption, retention,
  and deletion of stored documents.
- All examples in this repository use synthetic values.

## Development

```sh
gofmt -l .
go vet ./...
go test -race -count=1 -cover ./...
go build ./...
```

## License

MIT. Copyright (c) 2026 m1chlcz.
