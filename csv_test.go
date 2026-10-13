package invoiceledger_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/m1chlcz/invoice-ledger"
)

func testCSVInvoice() invoiceledger.Document {
	return invoiceledger.Document{
		ID:             uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		SequenceYear:   2026,
		SequenceNumber: 1,
		Number:         "FA-2026-000001",
		Kind:           invoiceledger.KindInvoice,
		OrderNumber:    "1001",
		Currency:       "CZK",
		ItemsMinor:     100000,
		ShippingMinor:  12000,
		DiscountMinor:  7000,
		TotalMinor:     105000,
		SupplyDate:     "2026-03-14",
		IssuedDate:     "2026-03-15",
		IssuedAt:       time.Date(2026, time.March, 15, 9, 0, 0, 0, time.UTC),
		Paid:           true,
		Reason:         `He said "thanks", twice`,
		Locale:         "cs-CZ",
		BuyerCountry:   "CZ",
		CreatedAt:      time.Date(2026, time.March, 15, 9, 0, 0, 123456000, time.UTC),
	}
}

func testCSVCredit() invoiceledger.Document {
	return invoiceledger.Document{
		ID:              uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		SequenceYear:    2026,
		SequenceNumber:  2,
		Number:          "OP-2026-000002",
		Kind:            invoiceledger.KindCreditNote,
		OrderNumber:     "1001",
		OriginalNumber:  "FA-2026-000001",
		RefundReference: "REF,001",
		RefundedOn:      "2026-03-16",
		Currency:        "CZK",
		ItemsMinor:      -50000,
		ShippingMinor:   0,
		DiscountMinor:   0,
		TotalMinor:      -50000,
		SupplyDate:      "2026-03-16",
		IssuedDate:      "2026-03-16",
		IssuedAt:        time.Date(2026, time.March, 16, 10, 30, 0, 0, time.UTC),
		Paid:            false,
		Reason:          "first line\nsecond line",
		Locale:          "en-GB",
		BuyerCountry:    "SK",
		CreatedAt:       time.Date(2026, time.March, 16, 10, 30, 0, 0, time.UTC),
	}
}

const testCSVHeader = "number,kind,sequence_year,sequence_number,order_number," +
	"original_number,refund_reference,refunded_on,currency,items_minor," +
	"shipping_minor,discount_minor,total_minor,supply_date,issued_date," +
	"issued_at,paid,reason,locale,buyer_country,id,created_at"

func TestExportCSVGoldenBytes(t *testing.T) {
	t.Parallel()

	invoice := testCSVInvoice()
	credit := testCSVCredit()
	var builder strings.Builder
	err := invoiceledger.ExportCSV(&builder, []invoiceledger.Document{credit, invoice})
	if err != nil {
		t.Fatalf("export csv: %v", err)
	}
	invoiceRow := `FA-2026-000001,invoice,2026,1,1001,,,,CZK,100000,12000,7000,105000,` +
		`2026-03-14,2026-03-15,2026-03-15T09:00:00Z,true,` +
		`"He said ""thanks"", twice",cs-CZ,CZ,11111111-1111-1111-1111-111111111111,` +
		`2026-03-15T09:00:00.123456Z`
	creditRow := "OP-2026-000002,credit_note,2026,2,1001,FA-2026-000001," +
		"\"REF,001\",2026-03-16,CZK,-50000,0,0,-50000,2026-03-16,2026-03-16," +
		"2026-03-16T10:30:00Z,false,\"first line\nsecond line\",en-GB,SK," +
		"22222222-2222-2222-2222-222222222222,2026-03-16T10:30:00Z"
	expected := testCSVHeader + "\n" + invoiceRow + "\n" + creditRow + "\n"
	if builder.String() != expected {
		t.Fatalf("csv bytes = %q, want %q", builder.String(), expected)
	}
}

func TestExportCSVSortsBySequenceThenID(t *testing.T) {
	t.Parallel()

	first := testCSVInvoice()
	first.SequenceNumber = 2
	first.ID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	second := testCSVInvoice()
	second.SequenceNumber = 2
	second.ID = uuid.MustParse("00000000-0000-0000-0000-000000000002")
	var builder strings.Builder
	if err := invoiceledger.ExportCSV(
		&builder,
		[]invoiceledger.Document{second, first},
	); err != nil {
		t.Fatalf("export csv: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(builder.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	if !strings.HasPrefix(lines[1], "FA-2026-000001,invoice,2026,2,") ||
		!strings.Contains(lines[1], "00000000-0000-0000-0000-000000000001") {
		t.Fatalf("first row = %q", lines[1])
	}
	if !strings.Contains(lines[2], "00000000-0000-0000-0000-000000000002") {
		t.Fatalf("second row = %q", lines[2])
	}
}

func TestExportCSVEmptyAndZeroFields(t *testing.T) {
	t.Parallel()

	var builder strings.Builder
	if err := invoiceledger.ExportCSV(&builder, nil); err != nil {
		t.Fatalf("empty export: %v", err)
	}
	if builder.String() != testCSVHeader+"\n" {
		t.Fatalf("empty csv = %q", builder.String())
	}
	sparse := invoiceledger.Document{
		ID:             uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		SequenceYear:   2025,
		SequenceNumber: 1,
		Number:         "FA-2025-000001",
		Kind:           invoiceledger.KindInvoice,
	}
	builder.Reset()
	if err := invoiceledger.ExportCSV(&builder, []invoiceledger.Document{sparse}); err != nil {
		t.Fatalf("sparse export: %v", err)
	}
	expectedRow := "FA-2025-000001,invoice,2025,1,,,,,,0,0,0,0,,,,false,,,," +
		"33333333-3333-3333-3333-333333333333,\n"
	expected := testCSVHeader + "\n" + expectedRow
	if builder.String() != expected {
		t.Fatalf("sparse csv = %q, want %q", builder.String(), expected)
	}
}

type failingCSVWriter struct {
	err error
}

func (writer failingCSVWriter) Write([]byte) (int, error) {
	return 0, writer.err
}

func TestExportCSVWriteFailure(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("disk full")
	err := invoiceledger.ExportCSV(failingCSVWriter{err: writeErr}, []invoiceledger.Document{
		testCSVInvoice(),
	})
	if !errors.Is(err, writeErr) {
		t.Fatalf("write error = %v, want %v", err, writeErr)
	}
}
