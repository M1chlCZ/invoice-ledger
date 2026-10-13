package invoiceledger

import (
	"bytes"
	"cmp"
	"encoding/csv"
	"io"
	"slices"
	"strconv"
	"time"
)

// csvColumns is the exported column order of ExportCSV.
var csvColumns = []string{
	"number",
	"kind",
	"sequence_year",
	"sequence_number",
	"order_number",
	"original_number",
	"refund_reference",
	"refunded_on",
	"currency",
	"items_minor",
	"shipping_minor",
	"discount_minor",
	"total_minor",
	"supply_date",
	"issued_date",
	"issued_at",
	"paid",
	"reason",
	"locale",
	"buyer_country",
	"id",
	"created_at",
}

// ExportCSV writes one RFC 4180 CSV row per document to w in a fixed column
// order: number, kind, sequence_year, sequence_number, order_number,
// original_number, refund_reference, refunded_on, currency, items_minor,
// shipping_minor, discount_minor, total_minor, supply_date, issued_date,
// issued_at, paid, reason, locale, buyer_country, id and created_at.
//
// The documents are copied and ordered by sequence year, sequence number and
// ID before they are written, so one document set always produces the same
// bytes regardless of the input order. Rows end with LF. Timestamps use
// RFC 3339 in UTC and a zero timestamp is empty. Fields are quoted only when
// RFC 4180 requires it.
func ExportCSV(w io.Writer, documents []Document) error {
	ordered := slices.Clone(documents)
	slices.SortStableFunc(ordered, compareDocuments)
	writer := csv.NewWriter(w)
	if err := writer.Write(csvColumns); err != nil {
		return err
	}
	for _, document := range ordered {
		if err := writer.Write(csvDocumentRow(document)); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

func compareDocuments(first, second Document) int {
	if order := cmp.Compare(first.SequenceYear, second.SequenceYear); order != 0 {
		return order
	}
	if order := cmp.Compare(first.SequenceNumber, second.SequenceNumber); order != 0 {
		return order
	}
	return bytes.Compare(first.ID[:], second.ID[:])
}

func csvDocumentRow(document Document) []string {
	return []string{
		document.Number,
		string(document.Kind),
		strconv.Itoa(document.SequenceYear),
		strconv.FormatInt(document.SequenceNumber, 10),
		document.OrderNumber,
		document.OriginalNumber,
		document.RefundReference,
		document.RefundedOn,
		document.Currency,
		strconv.FormatInt(document.ItemsMinor, 10),
		strconv.FormatInt(document.ShippingMinor, 10),
		strconv.FormatInt(document.DiscountMinor, 10),
		strconv.FormatInt(document.TotalMinor, 10),
		document.SupplyDate,
		document.IssuedDate,
		formatCSVTime(document.IssuedAt),
		strconv.FormatBool(document.Paid),
		document.Reason,
		document.Locale,
		document.BuyerCountry,
		document.ID.String(),
		formatCSVTime(document.CreatedAt),
	}
}

func formatCSVTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
