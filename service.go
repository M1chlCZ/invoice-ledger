package invoiceledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	// Embed the timezone database so the configured location always resolves.
	_ "time/tzdata"

	"github.com/google/uuid"

	"github.com/m1chlcz/invoice-ledger/vatkit"
)

const (
	dateLayout              = "2006-01-02"
	reasonMaxRunes          = 500
	refundReferenceMaxRunes = 200
	defaultInvoicePrefix    = "FA"
	defaultCreditPrefix     = "OP"
	defaultShippingLabel    = "Shipping"
	defaultCreditNoteLabel  = "Credit note for "
)

// Options configures a Service. Zero values select documented defaults.
type Options struct {
	// Source supplies order facts. It is required.
	Source Source

	// Store persists documents. It is required.
	Store Store

	// Clock returns the current time. The default is time.Now.
	Clock func() time.Time

	// Location resolves the numbering year and the issue date. The default
	// is Europe/Prague.
	Location *time.Location

	// Seller is the issuer identity that the Service freezes into each new
	// snapshot.
	Seller Seller

	// InvoicePrefix starts regular invoice numbers. The default is FA.
	InvoicePrefix string

	// CreditPrefix starts credit-note numbers. The default is OP.
	CreditPrefix string

	// ShippingLabel names the shipping line when the order facts name no
	// shipping method. The default is Shipping.
	ShippingLabel string

	// CreditNoteLabel starts the single credit-note line name. The default
	// is "Credit note for ".
	CreditNoteLabel string

	// Renderer is optional. Render returns ErrNoRenderer when it is nil.
	Renderer Renderer
}

// Service issues and reads immutable ledger documents. A Service is safe for
// concurrent use.
type Service struct {
	source          Source
	store           Store
	clock           func() time.Time
	location        *time.Location
	seller          Seller
	invoicePrefix   string
	creditPrefix    string
	shippingLabel   string
	creditNoteLabel string
	renderer        Renderer
	mutex           sync.Mutex
}

// New validates the options and returns a ready Service.
func New(options Options) (*Service, error) {
	if options.Store == nil {
		return nil, fmt.Errorf("invoiceledger: store is required: %w", ErrMisconfigured)
	}
	if options.Source == nil {
		return nil, fmt.Errorf("invoiceledger: source is required: %w", ErrMisconfigured)
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	location := options.Location
	if location == nil {
		loaded, err := time.LoadLocation("Europe/Prague")
		if err != nil {
			return nil, fmt.Errorf("invoiceledger: load default location: %w: %w", ErrMisconfigured, err)
		}
		location = loaded
	}
	service := &Service{
		source:          options.Source,
		store:           options.Store,
		clock:           clock,
		location:        location,
		seller:          options.Seller,
		invoicePrefix:   defaultIfEmpty(options.InvoicePrefix, defaultInvoicePrefix),
		creditPrefix:    defaultIfEmpty(options.CreditPrefix, defaultCreditPrefix),
		shippingLabel:   defaultIfEmpty(options.ShippingLabel, defaultShippingLabel),
		creditNoteLabel: defaultIfEmpty(options.CreditNoteLabel, defaultCreditNoteLabel),
		renderer:        options.Renderer,
	}
	return service, nil
}

// Get returns one stored document by ID.
func (service *Service) Get(ctx context.Context, id uuid.UUID) (Document, error) {
	document, err := service.store.ByID(ctx, id)
	if err != nil {
		return Document{}, err
	}
	return document, nil
}

// ListByOrder returns the invoice and every linked credit note for one order,
// in sequence year and sequence number order.
func (service *Service) ListByOrder(ctx context.Context, orderID uuid.UUID) ([]Document, error) {
	documents, err := service.store.ByOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	return documents, nil
}

// Render returns the bytes that the configured Renderer produces for one
// stored document.
func (service *Service) Render(ctx context.Context, id uuid.UUID) ([]byte, error) {
	if service.renderer == nil {
		return nil, ErrNoRenderer
	}
	document, err := service.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	payload, err := service.renderer.Render(ctx, document)
	if err != nil {
		return nil, fmt.Errorf("invoiceledger: render document: %w", err)
	}
	return payload, nil
}

func defaultIfEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func validDate(value string) bool {
	if value == "" {
		return true
	}
	parsed, err := time.Parse(dateLayout, value)
	return err == nil && parsed.Format(dateLayout) == value
}

func validText(value string, maxRunes int, required bool) bool {
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return false
	}
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= maxRunes
}

func payloadHash(parts ...string) []byte {
	hasher := sha256.New()
	for _, part := range parts {
		hasher.Write([]byte(part))
		hasher.Write([]byte{0})
	}
	return hasher.Sum(nil)
}

func checkIdempotency(
	storedKey *uuid.UUID,
	storedHash []byte,
	requestKey uuid.UUID,
	requestHash []byte,
) error {
	if storedKey == nil || requestKey == uuid.Nil || *storedKey != requestKey {
		return nil
	}
	if len(storedHash) == 0 || !bytes.Equal(storedHash, requestHash) {
		return ErrIdempotencyConflict
	}
	return nil
}

func optionalUUID(value uuid.UUID) *uuid.UUID {
	if value == uuid.Nil {
		return nil
	}
	return &value
}

func normalizeTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Microsecond)
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func addMinor(current, delta int64) (int64, bool) {
	if delta < 0 || current < 0 || current > math.MaxInt64-delta {
		return 0, false
	}
	return current + delta, true
}

func subMinor(current, delta int64) (int64, bool) {
	if delta < 0 || current < 0 {
		return 0, false
	}
	return current - delta, true
}

func reconciles(items, shipping, discount, total int64) bool {
	sum, ok := addMinor(items, shipping)
	if !ok {
		return false
	}
	sum, ok = subMinor(sum, discount)
	if !ok {
		return false
	}
	return sum == total
}

// reconcileItems validates every order line and the line sum. A line must
// satisfy Quantity*UnitPriceMinor == LineTotalMinor with a non-negative
// quantity and unit price, and the line totals must add up to itemsTotal.
func reconcileItems(items []OrderItem, itemsTotal int64) bool {
	var sum int64
	for _, item := range items {
		if item.Quantity < 0 || item.UnitPriceMinor < 0 {
			return false
		}
		if item.Quantity == 0 {
			if item.LineTotalMinor != 0 {
				return false
			}
			continue
		}
		lineTotal, err := vatkit.LineTotal(item.UnitPriceMinor, item.Quantity)
		if err != nil || lineTotal != item.LineTotalMinor {
			return false
		}
		sum, err = vatkit.Accumulate(sum, item.LineTotalMinor)
		if err != nil {
			return false
		}
	}
	return sum == itemsTotal
}

func legacyTax(facts OrderFacts) bool {
	for _, item := range facts.Items {
		if item.TaxRateBasisPoints != 0 || item.TaxMinor != 0 {
			return true
		}
	}
	return facts.ShippingTaxRateBasisPoints != 0 || facts.ShippingTaxMinor != 0
}
