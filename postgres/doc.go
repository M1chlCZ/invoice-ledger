// Package postgres provides a PostgreSQL adapter for the invoice ledger. It
// stores documents and the external turnover ledger in the shop schema and
// implements invoiceledger.Store, invoiceledger.CreditInserter,
// invoiceledger.TurnoverStore, invoiceledger.SettingsStore,
// invoiceledger.DocumentLister and invoiceledger.Source.
//
// The adapter never creates or changes schema objects. It requires the tables
// created by the shop migrations:
//
//   - shop.invoices
//   - shop.invoice_counters
//   - shop.invoice_settings
//   - shop.invoice_settings_requests
//   - shop.external_turnover_entries
//
// Source additionally reads shop.orders, shop.order_items and
// shop.order_shipping. The adapter writes no actor_user_id and keeps the
// shop.users foreign keys satisfied through the existing schema only.
//
// Documents store the snapshot in the flat shop shape (seller_name,
// buyer_name and the remaining flat party fields), so readers of the
// shop.invoices.snapshot column keep working. Buyer ID and phone have no
// column in that shape and are dropped on a round trip through this adapter.
// The shop schema accepts CZK documents only.
package postgres
