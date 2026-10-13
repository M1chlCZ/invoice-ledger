# Changelog

All notable changes to this package are documented in this file. The format
follows Keep a Changelog, and the project uses semantic versioning.

## [Unreleased]

### Added

- External turnover parity: `TurnoverEntry`, `TurnoverClassification`,
  `TurnoverRequest` and `TurnoverVoidRequest`, the optional `TurnoverStore`
  capability with `Service.CreateTurnover`, `Service.VoidTurnover` and
  `Service.ListTurnover`, and a `MemoryStore` implementation. Entries are
  append-only; a void appends a reversal with a negative amount and a link to
  the original row. Create and void are idempotent by key and payload hash and
  the store enforces one reversal per entry atomically.
- Invoicing settings parity: `Settings`, `SettingsUpdate` and the optional
  `SettingsStore` capability with `Service.GetSettings` and
  `Service.UpdateSettings`. Updates use optimistic versioning, record the
  applied outcome per idempotency key, and replay that outcome even after
  later updates. Defaults match the schema: `DefaultShopWarningMinor` and
  `DefaultExternalPlanningMinor`.
- The optional `DocumentLister` capability for reading the whole ledger.
- Deterministic CSV export: `ExportCSV` with a fixed column order, RFC 4180
  quoting, LF line endings and ordering by sequence year, sequence number and
  ID.
- The `postgres` subpackage: a `pgx`-based adapter implementing `Store`,
  `CreditInserter`, `TurnoverStore`, `SettingsStore`, `DocumentLister` and
  `Source` against the shop schema. It mirrors the shop's transactional
  behavior: counter allocation through one upsert, credit inserts with the
  original row locked, settings updates with the singleton locked, and
  turnover reversals derived from the locked target row. Documents keep the
  flat shop snapshot shape.
- The `cmd/ledgerctl` command: `export-csv` from PostgreSQL
  (`-database-url`, falling back to `INVOICELEDGER_DATABASE_URL`) or from a
  JSON document array (`-input`), writing to `-output` or standard output,
  with exit codes 0, 1 and 2.
- `ErrUnsupported` for Service calls that need a Store capability the
  configured store does not implement.
- PostgreSQL integration tests gated by `INVOICELEDGER_TEST_DATABASE_URL`,
  which reset a disposable database's `shop` schema and apply the invoice
  ledger migration.

### Changed

- A settings update without an idempotency key performs a plain
  version-checked update and writes no dedupe record; the schema's request log
  stays reserved for keyed updates.
