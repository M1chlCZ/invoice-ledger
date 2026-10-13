// Package postgres_test exercises the adapter against a disposable schema in a
// dedicated test database. Every scenario rebuilds the schema from the support
// stand-ins and the invoice ledger migration.
//
// The shared storetest conformance suite does not run here because
// shop.invoices references shop.orders: a document with a fresh order ID
// cannot be inserted without a seeded order row. The scenarios seed orders and
// cover the same operations end to end instead.
package postgres_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/m1chlcz/invoice-ledger"
	"github.com/m1chlcz/invoice-ledger/postgres"
)

const (
	testDatabaseEnv  = "INVOICELEDGER_TEST_DATABASE_URL"
	testMigrationEnv = "INVOICELEDGER_TEST_MIGRATION_SQL"
	testAddressJSON  = `{"first_name":"Test","last_name":"Buyer","street":"1 Example Street","city":"Example City","postal_code":"11000","country_code":"CZ"}`
	testSupplyDate   = "2026-03-14"
	testRefundDate   = "2026-03-16"
	testStatusPaid   = "paid"
	testOrderActive  = "completed"
	testMethodName   = "Example Carrier"
	testItemsTotal   = int64(100_000)
	testShippingCost = int64(12_000)
	testGrandTotal   = int64(112_000)
)

type seedOptions struct {
	itemsTotal       int64
	shippingTotal    int64
	discountTotal    int64
	grandTotal       int64
	methodName       string
	itemTaxRate      int32
	itemTaxMinor     int64
	shippingTaxRate  int32
	shippingTaxMinor int64
	orderStatus      string
	paymentStatus    string
}

func defaultSeed() seedOptions {
	return seedOptions{
		itemsTotal:    testItemsTotal,
		shippingTotal: testShippingCost,
		grandTotal:    testGrandTotal,
		methodName:    testMethodName,
		orderStatus:   testOrderActive,
		paymentStatus: testStatusPaid,
	}
}

func newIntegrationStore(t *testing.T) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv(testDatabaseEnv))
	if databaseURL == "" {
		t.Skip("set " + testDatabaseEnv + " to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	var databaseName string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
		t.Fatalf("read database name: %v", err)
	}
	if !strings.Contains(databaseName, "test") {
		t.Skipf(
			"refusing to reset the shop schema in database %q: the database name must contain test",
			databaseName,
		)
	}
	resetSchema(t, ctx, pool)
	return postgres.New(pool), pool
}

func resetSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS shop CASCADE"); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA shop"); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	support, err := os.ReadFile(filepath.Join("testdata", "support.sql"))
	if err != nil {
		t.Fatalf("read support.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, string(support)); err != nil {
		t.Fatalf("apply support.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, migrationSQL(t)); err != nil {
		t.Fatalf("apply invoice ledger migration: %v", err)
	}
}

// migrationSQL returns the invoice ledger migration. The embedded copy
// mirrors the shop migration; INVOICELEDGER_TEST_MIGRATION_SQL can point at
// the authoritative file instead.
func migrationSQL(t *testing.T) string {
	t.Helper()
	if path := strings.TrimSpace(os.Getenv(testMigrationEnv)); path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration %s: %v", path, err)
		}
		return string(content)
	}
	content, err := os.ReadFile(filepath.Join("testdata", "000106_invoice_ledger.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	return string(content)
}

func seedOrder(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	orderID uuid.UUID,
	orderNumber int64,
	options seedOptions,
) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO shop.orders (
			id, order_number, contact_email, email_locale, currency,
			billing_address, shipping_address, items_total_minor,
			shipping_total_minor, discount_total_minor, grand_total_minor,
			order_status, payment_status
		) VALUES ($1, $2, 'buyer@example.test', 'cs-CZ', 'CZK',
			$3::jsonb, $3::jsonb, $4, $5, $6, $7, $8, $9)`,
		orderID, orderNumber, testAddressJSON, options.itemsTotal,
		options.shippingTotal, options.discountTotal, options.grandTotal,
		options.orderStatus, options.paymentStatus,
	)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO shop.order_items (
			order_id, name, customization, quantity, unit_price_minor,
			line_total_minor, tax_rate_basis_points, tax_minor
		) VALUES ($1, 'Example product', '{"description":"  Blue   finish "}'::jsonb,
			1, $2, $2, $3, $4)`,
		orderID, options.itemsTotal, options.itemTaxRate, options.itemTaxMinor,
	)
	if err != nil {
		t.Fatalf("seed order item: %v", err)
	}
	if options.methodName != "" {
		_, err = pool.Exec(ctx, `
			INSERT INTO shop.order_shipping (
				order_id, method_name, tax_rate_basis_points, tax_minor
			) VALUES ($1, $2, $3, $4)`,
			orderID, options.methodName, options.shippingTaxRate, options.shippingTaxMinor,
		)
		if err != nil {
			t.Fatalf("seed order shipping: %v", err)
		}
	}
}

func newService(t *testing.T, store *postgres.Store) *invoiceledger.Service {
	t.Helper()
	fixed := time.Date(2026, time.March, 15, 9, 0, 0, 0, time.UTC)
	service, err := invoiceledger.New(invoiceledger.Options{
		Source:   store,
		Store:    store,
		Clock:    func() time.Time { return fixed },
		Location: time.UTC,
		Seller: invoiceledger.Seller{
			Name: "Example Seller", ID: "00000000",
			Address: "1 Example Street", Email: "seller@example.test",
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service
}

func TestPostgresIssueReplayAndCredit(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := context.Background()
	orderID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	seedOrder(t, ctx, pool, orderID, 1001, defaultSeed())
	service := newService(t, store)
	key := uuid.New()
	invoice, replayed, err := service.Issue(ctx, invoiceledger.IssueRequest{
		OrderID: orderID, SupplyDate: testSupplyDate, IdempotencyKey: key,
	})
	if err != nil || replayed {
		t.Fatalf("issue = %+v replayed=%t err=%v", invoice, replayed, err)
	}
	if invoice.Number != "FA-2026-000001" || invoice.Kind != invoiceledger.KindInvoice ||
		invoice.TotalMinor != testGrandTotal ||
		invoice.ItemsMinor != testItemsTotal ||
		invoice.ShippingMinor != testShippingCost ||
		invoice.OrderID == nil || *invoice.OrderID != orderID ||
		invoice.OrderNumber != "1001" || invoice.IssuedDate != "2026-03-15" ||
		invoice.SupplyDate != testSupplyDate || !invoice.Paid ||
		invoice.BuyerCountry != "CZ" || invoice.Locale != "cs-CZ" {
		t.Fatalf("issued invoice = %+v", invoice)
	}
	if len(invoice.Snapshot.Lines) != 2 {
		t.Fatalf("lines = %+v", invoice.Snapshot.Lines)
	}
	itemLine := invoice.Snapshot.Lines[0]
	if itemLine.Name != "Example product" || itemLine.Configuration != "Blue finish" ||
		itemLine.TotalMinor != testItemsTotal {
		t.Fatalf("item line = %+v", itemLine)
	}
	if invoice.Snapshot.Lines[1].Name != testMethodName {
		t.Fatalf("shipping line = %+v", invoice.Snapshot.Lines[1])
	}
	if invoice.Snapshot.Buyer.Name != "Test Buyer" ||
		invoice.Snapshot.Buyer.Address != "1 Example Street, Example City, 11000, CZ" ||
		invoice.Snapshot.Seller.Name != "Example Seller" {
		t.Fatalf("parties = %+v %+v", invoice.Snapshot.Seller, invoice.Snapshot.Buyer)
	}
	replay, replayed, err := service.Issue(ctx, invoiceledger.IssueRequest{
		OrderID: orderID, SupplyDate: testSupplyDate, IdempotencyKey: key,
	})
	if err != nil || !replayed || replay.ID != invoice.ID {
		t.Fatalf("replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
	if _, _, err := service.Issue(ctx, invoiceledger.IssueRequest{
		OrderID: orderID, SupplyDate: testSupplyDate, Reason: "changed",
		IdempotencyKey: key,
	}); !errors.Is(err, invoiceledger.ErrIdempotencyConflict) {
		t.Fatalf("payload conflict error = %v", err)
	}
	byOrder, replayed, err := service.Issue(ctx, invoiceledger.IssueRequest{
		OrderID: orderID, IdempotencyKey: uuid.New(),
	})
	if err != nil || !replayed || byOrder.ID != invoice.ID {
		t.Fatalf("order replay = %+v replayed=%t err=%v", byOrder, replayed, err)
	}
	byKey, err := store.ByIdempotencyKey(ctx, key)
	if err != nil || byKey.ID != invoice.ID {
		t.Fatalf("by idempotency key = %+v, %v", byKey, err)
	}
	fetched, err := store.ByID(ctx, invoice.ID)
	if err != nil || fetched.Number != invoice.Number || fetched.Snapshot.Buyer.Name != "Test Buyer" {
		t.Fatalf("by id = %+v, %v", fetched, err)
	}
	var buyerName, sellerName string
	err = pool.QueryRow(ctx, `
		SELECT snapshot->>'buyer_name', snapshot->>'seller_name'
		FROM shop.invoices WHERE id = $1`, invoice.ID).Scan(&buyerName, &sellerName)
	if err != nil || buyerName != "Test Buyer" || sellerName != "Example Seller" {
		t.Fatalf("shop snapshot projection = %q %q, %v", buyerName, sellerName, err)
	}
	duplicate := invoice
	duplicate.ID = uuid.New()
	duplicate.IdempotencyKey = nil
	if _, err := store.Insert(ctx, duplicate); !errors.Is(err, invoiceledger.ErrConflict) {
		t.Fatalf("duplicate number error = %v", err)
	}
	credit, replayed, err := service.Credit(ctx, invoiceledger.CreditRequest{
		InvoiceID: invoice.ID, AmountMinor: 50_000, Reason: "partial refund",
		RefundReference: "REF-1", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	})
	if err != nil || replayed {
		t.Fatalf("credit = %+v replayed=%t err=%v", credit, replayed, err)
	}
	if credit.Number != "OP-2026-000002" || credit.TotalMinor != -50_000 ||
		credit.OriginalID == nil || *credit.OriginalID != invoice.ID ||
		credit.OriginalNumber != invoice.Number || credit.RefundedOn != testRefundDate {
		t.Fatalf("credit document = %+v", credit)
	}
	credited, err := store.CreditedTotal(ctx, invoice.ID)
	if err != nil || credited != -50_000 {
		t.Fatalf("credited total = %d, %v", credited, err)
	}
	if _, _, err := service.Credit(ctx, invoiceledger.CreditRequest{
		InvoiceID: invoice.ID, AmountMinor: 70_000, Reason: "too much",
		RefundReference: "REF-2", RefundedOn: testRefundDate, IdempotencyKey: uuid.New(),
	}); !errors.Is(err, invoiceledger.ErrConflict) {
		t.Fatalf("over-credit error = %v", err)
	}
	documents, err := store.ByOrder(ctx, orderID)
	if err != nil || len(documents) != 2 {
		t.Fatalf("order documents = %d, %v", len(documents), err)
	}
}

func TestPostgresSourceEligibility(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := context.Background()
	legacyID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002")
	legacy := defaultSeed()
	legacy.itemTaxRate = 2100
	legacy.itemTaxMinor = 1
	seedOrder(t, ctx, pool, legacyID, 1002, legacy)
	unpaidID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000003")
	unpaid := defaultSeed()
	unpaid.paymentStatus = "pending"
	seedOrder(t, ctx, pool, unpaidID, 1003, unpaid)
	cancelledID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000004")
	cancelled := defaultSeed()
	cancelled.orderStatus = "cancelled"
	seedOrder(t, ctx, pool, cancelledID, 1004, cancelled)
	service := newService(t, store)
	if _, _, err := service.Issue(ctx, invoiceledger.IssueRequest{
		OrderID: legacyID, IdempotencyKey: uuid.New(),
	}); !errors.Is(err, invoiceledger.ErrLegacyTax) {
		t.Fatalf("legacy error = %v", err)
	}
	for _, orderID := range []uuid.UUID{unpaidID, cancelledID} {
		if _, _, err := service.Issue(ctx, invoiceledger.IssueRequest{
			OrderID: orderID, IdempotencyKey: uuid.New(),
		}); !errors.Is(err, invoiceledger.ErrConflict) {
			t.Fatalf("ineligible order error = %v", err)
		}
	}
	if _, err := store.Order(ctx, uuid.New()); !errors.Is(err, invoiceledger.ErrNotFound) {
		t.Fatalf("missing order error = %v", err)
	}
}

func TestPostgresTurnover(t *testing.T) {
	store, _ := newIntegrationStore(t)
	ceiling := newService(t, store)
	ctx := context.Background()
	key := uuid.New()
	request := invoiceledger.TurnoverRequest{
		OccurredOn: "2026-02-20", AmountMinor: 250_000, CountryCode: "DE",
		Classification: invoiceledger.TurnoverEUTBE, Note: "marketplace payout",
		IdempotencyKey: key,
	}
	entry, replayed, err := ceiling.CreateTurnover(ctx, request)
	if err != nil || replayed {
		t.Fatalf("create turnover = %+v replayed=%t err=%v", entry, replayed, err)
	}
	if entry.ID == uuid.Nil || entry.CreatedAt.IsZero() || entry.ReversalOf != nil ||
		entry.Classification != invoiceledger.TurnoverEUTBE {
		t.Fatalf("turnover entry = %+v", entry)
	}
	replay, replayed, err := ceiling.CreateTurnover(ctx, request)
	if err != nil || !replayed || replay.ID != entry.ID {
		t.Fatalf("turnover replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
	conflict := request
	conflict.AmountMinor = 250_001
	if _, _, err := ceiling.CreateTurnover(ctx, conflict); !errors.Is(
		err,
		invoiceledger.ErrIdempotencyConflict,
	) {
		t.Fatalf("turnover payload conflict error = %v", err)
	}
	year, err := ceiling.ListTurnover(ctx, 2026)
	if err != nil || len(year) != 1 || year[0].ID != entry.ID {
		t.Fatalf("year entries = %+v, %v", year, err)
	}
	previous, err := ceiling.ListTurnover(ctx, 2025)
	if err != nil || len(previous) != 0 {
		t.Fatalf("previous year = %+v, %v", previous, err)
	}
	reversal, replayed, err := ceiling.VoidTurnover(ctx, invoiceledger.TurnoverVoidRequest{
		EntryID: entry.ID, Reason: "duplicate entry", IdempotencyKey: uuid.New(),
	})
	if err != nil || replayed {
		t.Fatalf("void = %+v replayed=%t err=%v", reversal, replayed, err)
	}
	if reversal.ReversalOf == nil || *reversal.ReversalOf != entry.ID ||
		reversal.AmountMinor != -250_000 || reversal.Reason != "duplicate entry" ||
		reversal.OccurredOn != entry.OccurredOn ||
		reversal.Classification != entry.Classification {
		t.Fatalf("reversal = %+v", reversal)
	}
	if _, _, err := ceiling.VoidTurnover(ctx, invoiceledger.TurnoverVoidRequest{
		EntryID: entry.ID, Reason: "void again", IdempotencyKey: uuid.New(),
	}); !errors.Is(err, invoiceledger.ErrConflict) {
		t.Fatalf("double void error = %v", err)
	}
	if _, _, err := ceiling.VoidTurnover(ctx, invoiceledger.TurnoverVoidRequest{
		EntryID: uuid.New(), Reason: "missing", IdempotencyKey: uuid.New(),
	}); !errors.Is(err, invoiceledger.ErrNotFound) {
		t.Fatalf("missing target error = %v", err)
	}
	all, err := ceiling.ListTurnover(ctx, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("all entries = %+v, %v", all, err)
	}
	fetched, err := store.TurnoverByID(ctx, reversal.ID)
	if err != nil || fetched.AmountMinor != reversal.AmountMinor {
		t.Fatalf("fetch reversal = %+v, %v", fetched, err)
	}
}

func TestPostgresSettings(t *testing.T) {
	store, _ := newIntegrationStore(t)
	service := newService(t, store)
	ctx := context.Background()
	settings, err := service.GetSettings(ctx)
	if err != nil || settings.Version != 1 ||
		settings.ShopWarningMinor != invoiceledger.DefaultShopWarningMinor ||
		settings.ExternalPlanningMinor != invoiceledger.DefaultExternalPlanningMinor {
		t.Fatalf("default settings = %+v, %v", settings, err)
	}
	key := uuid.New()
	update := invoiceledger.SettingsUpdate{
		ShopWarningMinor: 5_000, ExternalPlanningMinor: 7_000,
		Version: 1, IdempotencyKey: key,
	}
	updated, replayed, err := service.UpdateSettings(ctx, update)
	if err != nil || replayed || updated.Version != 2 ||
		updated.ShopWarningMinor != 5_000 || updated.ExternalPlanningMinor != 7_000 {
		t.Fatalf("update settings = %+v replayed=%t err=%v", updated, replayed, err)
	}
	replay, replayed, err := service.UpdateSettings(ctx, update)
	if err != nil || !replayed || replay != updated {
		t.Fatalf("settings replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
	conflict := update
	conflict.ShopWarningMinor = 5_001
	if _, _, err := service.UpdateSettings(ctx, conflict); !errors.Is(
		err,
		invoiceledger.ErrIdempotencyConflict,
	) {
		t.Fatalf("settings payload conflict error = %v", err)
	}
	stale := invoiceledger.SettingsUpdate{
		ShopWarningMinor: 6_000, ExternalPlanningMinor: 8_000,
		Version: 1, IdempotencyKey: uuid.New(),
	}
	if _, _, err := service.UpdateSettings(ctx, stale); !errors.Is(err, invoiceledger.ErrConflict) {
		t.Fatalf("stale settings error = %v", err)
	}
	keyless := invoiceledger.SettingsUpdate{
		ShopWarningMinor: 6_000, ExternalPlanningMinor: 8_000, Version: 2,
	}
	applied, replayed, err := service.UpdateSettings(ctx, keyless)
	if err != nil || replayed || applied.Version != 3 {
		t.Fatalf("keyless update = %+v replayed=%t err=%v", applied, replayed, err)
	}
	keyless.Version = 3
	applied, replayed, err = service.UpdateSettings(ctx, keyless)
	if err != nil || replayed || applied.Version != 4 {
		t.Fatalf("second keyless update = %+v replayed=%t err=%v", applied, replayed, err)
	}
	current, err := service.GetSettings(ctx)
	if err != nil || current.Version != 4 || current.ShopWarningMinor != 6_000 {
		t.Fatalf("current settings = %+v, %v", current, err)
	}
	replay, replayed, err = service.UpdateSettings(ctx, update)
	if err != nil || !replayed || replay.Version != 2 {
		t.Fatalf("early replay = %+v replayed=%t err=%v", replay, replayed, err)
	}
}

func TestPostgresConcurrentAllocationAndTurnoverKey(t *testing.T) {
	store, _ := newIntegrationStore(t)
	ctx := context.Background()
	const workers = 16
	numbers := make(chan int64, workers)
	var waitGroup sync.WaitGroup
	for range workers {
		waitGroup.Go(func() {
			sequence, err := store.AllocateNumber(ctx, 2026, invoiceledger.KindInvoice)
			if err != nil {
				numbers <- 0
				return
			}
			numbers <- sequence.Number
		})
	}
	waitGroup.Wait()
	close(numbers)
	seen := make(map[int64]struct{}, workers)
	for number := range numbers {
		if number == 0 {
			t.Fatal("concurrent allocation failed")
		}
		if _, duplicate := seen[number]; duplicate {
			t.Fatalf("duplicate allocation %d", number)
		}
		seen[number] = struct{}{}
	}
	for expected := int64(1); expected <= workers; expected++ {
		if _, exists := seen[expected]; !exists {
			t.Fatalf("missing allocation %d", expected)
		}
	}
	service := newService(t, store)
	key := uuid.New()
	request := invoiceledger.TurnoverRequest{
		OccurredOn: "2026-04-01", AmountMinor: 1_000,
		Classification: invoiceledger.TurnoverDomestic, IdempotencyKey: key,
	}
	type outcome struct {
		entry    invoiceledger.TurnoverEntry
		replayed bool
		err      error
	}
	results := make(chan outcome, 2)
	var createGroup sync.WaitGroup
	for range 2 {
		createGroup.Go(func() {
			entry, replayed, err := service.CreateTurnover(ctx, request)
			results <- outcome{entry: entry, replayed: replayed, err: err}
		})
	}
	createGroup.Wait()
	close(results)
	replays := 0
	var firstID uuid.UUID
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent turnover error = %v", result.err)
		}
		if firstID == uuid.Nil {
			firstID = result.entry.ID
		}
		if result.entry.ID != firstID {
			t.Fatalf("concurrent turnover ids = %s, %s", result.entry.ID, firstID)
		}
		if result.replayed {
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("replays = %d, want 1", replays)
	}
}

func TestPostgresListDocumentsAndCSV(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := context.Background()
	firstID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000005")
	secondID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000006")
	seedOrder(t, ctx, pool, firstID, 1005, defaultSeed())
	seedOrder(t, ctx, pool, secondID, 1006, defaultSeed())
	service := newService(t, store)
	if _, _, err := service.Issue(ctx, invoiceledger.IssueRequest{
		OrderID: firstID, IdempotencyKey: uuid.New(),
	}); err != nil {
		t.Fatalf("first issue: %v", err)
	}
	if _, _, err := service.Issue(ctx, invoiceledger.IssueRequest{
		OrderID: secondID, IdempotencyKey: uuid.New(),
	}); err != nil {
		t.Fatalf("second issue: %v", err)
	}
	documents, err := store.ListDocuments(ctx)
	if err != nil || len(documents) != 2 {
		t.Fatalf("list documents = %d, %v", len(documents), err)
	}
	if documents[0].Number != "FA-2026-000001" || documents[1].Number != "FA-2026-000002" {
		t.Fatalf("document order = %s, %s", documents[0].Number, documents[1].Number)
	}
	var builder strings.Builder
	if err := invoiceledger.ExportCSV(&builder, documents); err != nil {
		t.Fatalf("export csv: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(builder.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("csv lines = %d", len(lines))
	}
	if !strings.Contains(lines[0], "number,kind,sequence_year") ||
		!strings.HasPrefix(lines[1], "FA-2026-000001,invoice,2026,1,") ||
		!strings.HasPrefix(lines[2], "FA-2026-000002,invoice,2026,2,") {
		t.Fatalf("csv = %q", builder.String())
	}
	store2, err := postgres.Open(ctx, os.Getenv(testDatabaseEnv))
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	defer store2.Close()
	documents2, err := store2.ListDocuments(ctx)
	if err != nil || len(documents2) != 2 {
		t.Fatalf("second store documents = %d, %v", len(documents2), err)
	}
	if documents2[0].ID != documents[0].ID {
		t.Fatalf("second store ids differ")
	}
}
