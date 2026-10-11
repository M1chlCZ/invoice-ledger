package invoiceledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Kind classifies one append-only ledger document.
type Kind string

const (
	// KindInvoice is a regular invoice issued for an eligible paid order.
	KindInvoice Kind = "invoice"

	// KindCreditNote is a correcting document linked to one original invoice.
	KindCreditNote Kind = "credit_note"
)

// Line is one frozen document line. Amounts stay in integer minor units and
// are never re-read from an external catalog.
type Line struct {
	Name           string `json:"name"`
	Configuration  string `json:"configuration,omitempty"`
	Quantity       int64  `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	TotalMinor     int64  `json:"total_minor"`
}

// Party is a frozen seller or buyer identity.
type Party struct {
	Name    string `json:"name"`
	ID      string `json:"id,omitempty"`
	Address string `json:"address"`
	Email   string `json:"email,omitempty"`
	Phone   string `json:"phone,omitempty"`
}

// Seller is the issuer identity that the Service copies into every new
// snapshot. The caller owns the values.
type Seller struct {
	Name    string
	ID      string
	Address string
	Email   string
	Phone   string
}

// Snapshot is the frozen document content. A stored snapshot never changes
// when order, catalog, or configuration data changes later.
type Snapshot struct {
	Number         string `json:"number"`
	Kind           Kind   `json:"kind"`
	OriginalNumber string `json:"original_number,omitempty"`
	IssuedDate     string `json:"issued_date"`
	SupplyDate     string `json:"supply_date,omitempty"`
	OrderNumber    string `json:"order_number"`
	Locale         string `json:"locale"`
	Currency       string `json:"currency"`
	Seller         Party  `json:"seller"`
	Buyer          Party  `json:"buyer"`
	Lines          []Line `json:"lines"`
	ItemsMinor     int64  `json:"items_minor"`
	ShippingMinor  int64  `json:"shipping_minor"`
	DiscountMinor  int64  `json:"discount_minor"`
	TotalMinor     int64  `json:"total_minor"`
	Reason         string `json:"reason,omitempty"`
	Paid           bool   `json:"paid"`
}

// Document is one append-only ledger row with its frozen Snapshot. The
// ledger never updates or deletes issued documents.
type Document struct {
	ID                     uuid.UUID  `json:"id"`
	SequenceYear           int        `json:"sequence_year"`
	SequenceNumber         int64      `json:"sequence_number"`
	Number                 string     `json:"number"`
	Kind                   Kind       `json:"kind"`
	OrderID                *uuid.UUID `json:"order_id,omitempty"`
	OrderNumber            string     `json:"order_number,omitempty"`
	OriginalID             *uuid.UUID `json:"original_id,omitempty"`
	OriginalNumber         string     `json:"original_number,omitempty"`
	RefundReference        string     `json:"refund_reference,omitempty"`
	RefundedOn             string     `json:"refunded_on,omitempty"`
	Currency               string     `json:"currency"`
	ItemsMinor             int64      `json:"items_minor"`
	ShippingMinor          int64      `json:"shipping_minor"`
	DiscountMinor          int64      `json:"discount_minor"`
	TotalMinor             int64      `json:"total_minor"`
	SupplyDate             string     `json:"supply_date,omitempty"`
	IssuedDate             string     `json:"issued_date"`
	IssuedAt               time.Time  `json:"issued_at"`
	Paid                   bool       `json:"paid"`
	Reason                 string     `json:"reason,omitempty"`
	Locale                 string     `json:"locale"`
	BuyerCountry           string     `json:"buyer_country,omitempty"`
	Snapshot               Snapshot   `json:"snapshot"`
	CreatedAt              time.Time  `json:"created_at"`
	IdempotencyKey         *uuid.UUID `json:"idempotency_key,omitempty"`
	IdempotencyPayloadHash []byte     `json:"idempotency_payload_hash,omitempty"`
}

// Sequence is one allocated position in a calendar-year numbering series.
// Numbers start at one and increase by one across both kinds.
type Sequence struct {
	Year   int   `json:"year"`
	Kind   Kind  `json:"kind"`
	Number int64 `json:"number"`
}

// Format renders the full document number for a series prefix.
func (sequence Sequence) Format(prefix string) string {
	return fmt.Sprintf("%s-%d-%06d", prefix, sequence.Year, sequence.Number)
}

// Address is one frozen postal address from the order source.
type Address struct {
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
	Company     string `json:"company,omitempty"`
	Street      string `json:"street"`
	City        string `json:"city"`
	PostalCode  string `json:"postal_code"`
	CountryCode string `json:"country_code"`
}

// OrderItem is one frozen order line from the order source. The
// configuration is a human-readable summary; the ledger collapses repeated
// whitespace. The tax fields mark legacy orders that carry tax data.
type OrderItem struct {
	Name               string `json:"name"`
	Configuration      string `json:"configuration,omitempty"`
	Quantity           int64  `json:"quantity"`
	UnitPriceMinor     int64  `json:"unit_price_minor"`
	LineTotalMinor     int64  `json:"line_total_minor"`
	TaxRateBasisPoints int32  `json:"tax_rate_basis_points"`
	TaxMinor           int64  `json:"tax_minor"`
}

// OrderFacts is the immutable order state the ledger needs at issuance time.
// The Source supplies all fields; the ledger never reads a database.
type OrderFacts struct {
	OrderID                    uuid.UUID   `json:"order_id"`
	OrderNumber                int64       `json:"order_number"`
	Currency                   string      `json:"currency"`
	Locale                     string      `json:"locale"`
	ContactEmail               string      `json:"contact_email"`
	BillingAddress             Address     `json:"billing_address"`
	ShippingAddress            Address     `json:"shipping_address"`
	ItemsTotalMinor            int64       `json:"items_total_minor"`
	ShippingTotalMinor         int64       `json:"shipping_total_minor"`
	DiscountTotalMinor         int64       `json:"discount_total_minor"`
	GrandTotalMinor            int64       `json:"grand_total_minor"`
	Items                      []OrderItem `json:"items"`
	ShippingMethodName         string      `json:"shipping_method_name,omitempty"`
	HasShippingMethod          bool        `json:"has_shipping_method,omitempty"`
	ShippingTaxRateBasisPoints int32       `json:"shipping_tax_rate_basis_points"`
	ShippingTaxMinor           int64       `json:"shipping_tax_minor"`
	Cancelled                  bool        `json:"cancelled"`
	Paid                       bool        `json:"paid"`
}

// Source supplies order facts to the Service. Order returns ErrNotFound when
// the order does not exist. The ledger propagates any other error to the
// caller unchanged.
type Source interface {
	Order(ctx context.Context, orderID uuid.UUID) (OrderFacts, error)
}

// Renderer turns one stored document into bytes. Consumers can plug the
// published invoice PDF renderer without coupling this module to it.
type Renderer interface {
	Render(ctx context.Context, document Document) ([]byte, error)
}
