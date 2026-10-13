package postgres

import (
	"strings"
	"testing"

	"github.com/m1chlcz/invoice-ledger"
)

func TestSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	snapshot := invoiceledger.Snapshot{
		Number:        "FA-2026-000001",
		Kind:          invoiceledger.KindInvoice,
		IssuedDate:    "2026-03-15",
		SupplyDate:    "2026-03-14",
		OrderNumber:   "1001",
		Locale:        "cs-CZ",
		Currency:      "CZK",
		Reason:        "manual issue",
		Paid:          true,
		ItemsMinor:    1000,
		ShippingMinor: 100,
		DiscountMinor: 0,
		TotalMinor:    1100,
		Seller:        invoiceledger.Party{Name: "Example Seller", ID: "00000000", Address: "1 Example Street", Email: "seller@example.test", Phone: "+420000000000"},
		Buyer:         invoiceledger.Party{Name: "Test Buyer", Address: "1 Example Street, CZ", Email: "buyer@example.test"},
		Lines:         []invoiceledger.Line{{Name: "Example product", Quantity: 1, UnitPriceMinor: 1000, TotalMinor: 1000}},
	}
	encoded, err := encodeSnapshot(snapshot)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	text := string(encoded)
	for _, fragment := range []string{
		`"seller_name":"Example Seller"`,
		`"seller_id":"00000000"`,
		`"buyer_name":"Test Buyer"`,
		`"buyer_address":"1 Example Street, CZ"`,
		`"buyer_email":"buyer@example.test"`,
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("encoded snapshot %s misses %s", text, fragment)
		}
	}
	decoded, err := decodeSnapshot(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Number != snapshot.Number || decoded.Kind != snapshot.Kind ||
		decoded.IssuedDate != snapshot.IssuedDate || decoded.SupplyDate != snapshot.SupplyDate ||
		decoded.OrderNumber != snapshot.OrderNumber || decoded.Locale != snapshot.Locale ||
		decoded.Currency != snapshot.Currency || decoded.Reason != snapshot.Reason ||
		decoded.Paid != snapshot.Paid || decoded.ItemsMinor != snapshot.ItemsMinor ||
		decoded.ShippingMinor != snapshot.ShippingMinor ||
		decoded.DiscountMinor != snapshot.DiscountMinor ||
		decoded.TotalMinor != snapshot.TotalMinor {
		t.Fatalf("decoded totals = %+v", decoded)
	}
	if decoded.Seller != snapshot.Seller {
		t.Fatalf("decoded seller = %+v, want %+v", decoded.Seller, snapshot.Seller)
	}
	if decoded.Buyer != snapshot.Buyer {
		t.Fatalf("decoded buyer = %+v, want %+v", decoded.Buyer, snapshot.Buyer)
	}
	if len(decoded.Lines) != 1 || decoded.Lines[0].Name != "Example product" {
		t.Fatalf("decoded lines = %+v", decoded.Lines)
	}
	empty, err := decodeSnapshot(nil)
	if err != nil || empty.Number != "" {
		t.Fatalf("empty snapshot = %+v, %v", empty, err)
	}
	if _, err := decodeSnapshot([]byte("not json")); err == nil {
		t.Fatal("invalid snapshot did not fail")
	}
}
