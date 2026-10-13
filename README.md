# invoice-ledger

`invoice-ledger` is a pure-domain, append-only invoice ledger for Go. The core
package has no database, HTTP, or rendering code; the caller supplies order
facts and storage. A separate `postgres` subpackage adapts the ledger to the
PostgreSQL schema it was extracted from, and `cmd/ledgerctl` exports the ledger
to CSV.

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
- Tracks external turnover entries with idempotent create and void operations.
- Keeps a versioned monitoring settings singleton.
- Exports the ledger as deterministic RFC 4180 CSV.

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

## External turnover and settings

The optional `TurnoverStore` and `SettingsStore` capabilities extend a `Store`
with the monitoring side of the ledger. `MemoryStore` and the `postgres`
adapter implement both. `Service.CreateTurnover`, `Service.VoidTurnover`,
`Service.ListTurnover`, `Service.GetSettings` and `Service.UpdateSettings`
return `ErrUnsupported` for a store without the capability.

### External turnover

`TurnoverEntry` is one append-only manual turnover row.
`TurnoverClassification` is one of `pending`, `domestic`, `eu_tbe` and
`excluded`. A void appends a separate reversal row with a negative amount and
`ReversalOf` set; the original row is never changed.

| Operation | Contract |
| --- | --- |
| `CreateTurnover(ctx, TurnoverRequest)` | Appends a positive entry with a required occurrence date, a valid classification and an empty or two-letter uppercase country code. The note is optional and limited to 500 runes. Idempotent by key and payload hash. |
| `VoidTurnover(ctx, TurnoverVoidRequest)` | Appends the reversal of one entry and requires a reason. A missing entry returns `ErrNotFound`; a reversal, a non-positive entry or an already reversed entry returns `ErrConflict`. Idempotent by key and payload hash. |
| `ListTurnover(ctx, year)` | Returns one calendar year of entries ordered by creation time descending and then by ID ascending. Year zero returns every entry. |

A `TurnoverStore` enforces the "one reversal per entry" rule atomically,
including across processes that share the store.

### Settings

`Settings` is the singleton monitoring configuration: `ShopWarningMinor`,
`ExternalPlanningMinor`, `Version` and `UpdatedAt`. Numbering is not a
setting; `Store.AllocateNumber` owns the year-bound sequence series.
`UpdateSettings` uses optimistic versioning. With a non-nil idempotency key the
applied outcome is recorded, so a replay returns that outcome even after later
updates changed the live singleton; a stale version returns `ErrConflict`. The
memory store starts at the documented defaults `DefaultShopWarningMinor` and
`DefaultExternalPlanningMinor` with version one.

## CSV export

`ExportCSV(w, documents)` writes one RFC 4180 row per document with a fixed
header, LF line endings, plain integer minor units and RFC 3339 UTC
timestamps. The input is copied and ordered by sequence year, sequence number
and ID, so the same document set always produces the same bytes. Empty fields
are empty cells; quoting is applied only when RFC 4180 requires it.

The columns, in order: `number`, `kind`, `sequence_year`, `sequence_number`,
`order_number`, `original_number`, `refund_reference`, `refunded_on`,
`currency`, `items_minor`, `shipping_minor`, `discount_minor`, `total_minor`,
`supply_date`, `issued_date`, `issued_at`, `paid`, `reason`, `locale`,
`buyer_country`, `id`, `created_at`.

The optional `DocumentLister` capability reads the whole ledger for export.
`postgres.Store` implements it.

## PostgreSQL adapter

`postgres.Open(ctx, databaseURL)` connects and returns a `*postgres.Store`
that implements `Store`, `CreditInserter`, `TurnoverStore`, `SettingsStore`,
`DocumentLister` and `Source` against the shop schema. `postgres.New(pool)`
wraps an existing `*pgxpool.Pool`. The adapter never creates or changes schema
objects.

Required tables:

| Table | Used by |
| --- | --- |
| `shop.invoices` | documents |
| `shop.invoice_counters` | number allocation |
| `shop.invoice_settings` | settings singleton |
| `shop.invoice_settings_requests` | settings update deduplication |
| `shop.external_turnover_entries` | external turnover |
| `shop.orders`, `shop.order_items`, `shop.order_shipping` | `Source` order facts |

Mapping decisions:

- Documents keep the flat snapshot shape of the shop column, so
  `snapshot->>'buyer_name'` and the other flat party keys keep working. Buyer
  ID and phone have no column in that shape and are dropped on a round trip.
- The schema accepts CZK documents only and enforces its own amount, date and
  reconciliation constraints. Violations surface as `ErrUnprocessable`;
  uniqueness and foreign-key conflicts surface as `ErrConflict`.
- `InsertCredit` locks the original invoice row, sums the linked credit notes
  and inserts in one transaction, so two processes cannot over-credit.
- `UpdateSettings` locks the singleton before the version check and rechecks a
  concurrent request with the same key before reporting a version conflict. A
  nil idempotency key applies a plain version-checked update and is not
  recorded in the dedupe log.
- `AllocateNumber` is one upsert on `shop.invoice_counters`. Allocation and
  insert stay separate, so numbering gaps remain possible by design.
- The adapter writes no `actor_user_id` and stores anonymous ledger rows.

Integration tests are skipped unless `INVOICELEDGER_TEST_DATABASE_URL` is set.
They reset the `shop` schema of a disposable database and apply the invoice
ledger migration, so point the variable only at a database whose name contains
`test`:

```sh
INVOICELEDGER_TEST_DATABASE_URL=postgres://user@127.0.0.1:5432/invoiceledger_test \
  go test -race -count=1 ./...
```

The tests carry a byte-identical copy of the migration. Set
`INVOICELEDGER_TEST_MIGRATION_SQL` to an authoritative migration path to apply
that file instead.

## Command line

`cmd/ledgerctl` exports the ledger as CSV from PostgreSQL or from a JSON
document array in the stable `Document` JSON mapping:

```sh
ledgerctl export-csv -database-url "$DATABASE_URL" -output ledger.csv
ledgerctl export-csv -input documents.json
```

`-database-url` falls back to `INVOICELEDGER_DATABASE_URL`. Exactly one of
`-database-url` and `-input` is required; `-input` wins when the environment
variable is set but the flag is not. `-output` defaults to standard output.
Exit codes are 0 for success, 1 for a runtime failure and 2 for a usage error.

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
- The core package makes no network calls and collects no telemetry. The
  `postgres` adapter connects only to the database URL the caller supplies,
  and `cmd/ledgerctl` only reads the input it is given.
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

The `postgres` integration tests run only when `INVOICELEDGER_TEST_DATABASE_URL`
points at a disposable database whose name contains `test`; see the
PostgreSQL adapter section.

## License

MIT. Copyright (c) 2026 m1chlcz.
