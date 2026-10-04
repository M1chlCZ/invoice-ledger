package invoiceledger

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// IssueRequest is one explicit, idempotent issuance request for an eligible
// paid order.
type IssueRequest struct {
	OrderID        uuid.UUID
	SupplyDate     string
	Reason         string
	IdempotencyKey uuid.UUID
}

// Issue creates the single invoice for an eligible paid order. Issue is
// idempotent by key and payload hash and is safe to replay.
//
// A repeated key with the same payload returns the stored document with
// replayed true. The same key with a different payload returns
// ErrIdempotencyConflict. An order that already has an invoice returns that
// invoice with replayed true unless the stored key matches the request key
// with a different payload. Cancelled or unpaid orders return ErrConflict.
// Orders that carry tax data return ErrLegacyTax.
func (service *Service) Issue(ctx context.Context, request IssueRequest) (Document, bool, error) {
	if !validDate(request.SupplyDate) || !validText(request.Reason, reasonMaxRunes, false) {
		return Document{}, false, ErrUnprocessable
	}
	now := service.clock()
	hash := payloadHash(
		"issue", request.OrderID.String(), request.SupplyDate, request.Reason,
	)
	facts, err := service.source.Order(ctx, request.OrderID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Document{}, false, ErrNotFound
		}
		return Document{}, false, err
	}
	if facts.OrderID != request.OrderID {
		return Document{}, false, ErrUnprocessable
	}
	documents, err := service.store.ByOrder(ctx, request.OrderID)
	if err != nil {
		return Document{}, false, err
	}
	for _, document := range documents {
		if document.Kind != KindInvoice {
			continue
		}
		if idempotencyErr := checkIdempotency(
			document.IdempotencyKey, document.IdempotencyPayloadHash,
			request.IdempotencyKey, hash,
		); idempotencyErr != nil {
			return Document{}, false, idempotencyErr
		}
		return document, true, nil
	}
	if facts.Cancelled || !facts.Paid {
		return Document{}, false, ErrConflict
	}
	if legacyTax(facts) {
		return Document{}, false, ErrLegacyTax
	}
	document, err := service.buildInvoice(ctx, facts, request, hash, now)
	if err != nil {
		return Document{}, false, err
	}
	stored, err := service.store.Insert(ctx, document)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return service.replayIssue(ctx, request, hash)
		}
		return Document{}, false, err
	}
	return stored, false, nil
}

func (service *Service) buildInvoice(
	ctx context.Context,
	facts OrderFacts,
	request IssueRequest,
	hash []byte,
	now time.Time,
) (Document, error) {
	if !reconcileItems(facts.Items, facts.ItemsTotalMinor) {
		return Document{}, ErrUnprocessable
	}
	if !reconciles(
		facts.ItemsTotalMinor, facts.ShippingTotalMinor,
		facts.DiscountTotalMinor, facts.GrandTotalMinor,
	) {
		return Document{}, ErrUnprocessable
	}
	if facts.GrandTotalMinor < 0 {
		return Document{}, ErrUnprocessable
	}
	buyer, billingCountry := buyerFromFacts(facts)
	destination := deliveryCountry(facts)
	if destination == "" {
		destination = billingCountry
	}
	issued := now.In(service.location)
	sequence, err := service.store.AllocateNumber(ctx, issued.Year(), KindInvoice)
	if err != nil {
		return Document{}, err
	}
	number := sequence.Format(service.invoicePrefix)
	snapshot := Snapshot{
		Number:        number,
		Kind:          KindInvoice,
		IssuedDate:    issued.Format(dateLayout),
		SupplyDate:    request.SupplyDate,
		OrderNumber:   strconv.FormatInt(facts.OrderNumber, 10),
		Locale:        facts.Locale,
		Currency:      facts.Currency,
		Seller:        service.sellerParty(),
		Buyer:         buyer,
		Lines:         service.invoiceLines(facts),
		ItemsMinor:    facts.ItemsTotalMinor,
		ShippingMinor: facts.ShippingTotalMinor,
		DiscountMinor: facts.DiscountTotalMinor,
		TotalMinor:    facts.GrandTotalMinor,
		Reason:        request.Reason,
		Paid:          true,
	}
	orderID := facts.OrderID
	document := Document{
		ID:                     uuid.New(),
		SequenceYear:           sequence.Year,
		SequenceNumber:         sequence.Number,
		Number:                 number,
		Kind:                   KindInvoice,
		OrderID:                &orderID,
		OrderNumber:            snapshot.OrderNumber,
		Currency:               facts.Currency,
		ItemsMinor:             facts.ItemsTotalMinor,
		ShippingMinor:          facts.ShippingTotalMinor,
		DiscountMinor:          facts.DiscountTotalMinor,
		TotalMinor:             facts.GrandTotalMinor,
		SupplyDate:             request.SupplyDate,
		IssuedDate:             snapshot.IssuedDate,
		IssuedAt:               normalizeTime(now),
		Paid:                   true,
		Reason:                 request.Reason,
		Locale:                 facts.Locale,
		BuyerCountry:           destination,
		Snapshot:               snapshot,
		CreatedAt:              normalizeTime(now),
		IdempotencyKey:         optionalUUID(request.IdempotencyKey),
		IdempotencyPayloadHash: hash,
	}
	return document, nil
}

func (service *Service) replayIssue(
	ctx context.Context,
	request IssueRequest,
	hash []byte,
) (Document, bool, error) {
	if request.IdempotencyKey != uuid.Nil {
		existing, err := service.store.ByIdempotencyKey(ctx, request.IdempotencyKey)
		switch {
		case err == nil:
			if idempotencyErr := checkIdempotency(
				existing.IdempotencyKey, existing.IdempotencyPayloadHash,
				request.IdempotencyKey, hash,
			); idempotencyErr != nil {
				return Document{}, false, idempotencyErr
			}
			return existing, true, nil
		case errors.Is(err, ErrNotFound):
		default:
			return Document{}, false, err
		}
	}
	documents, err := service.store.ByOrder(ctx, request.OrderID)
	if err != nil {
		return Document{}, false, err
	}
	for _, document := range documents {
		if document.Kind != KindInvoice {
			continue
		}
		if idempotencyErr := checkIdempotency(
			document.IdempotencyKey, document.IdempotencyPayloadHash,
			request.IdempotencyKey, hash,
		); idempotencyErr != nil {
			return Document{}, false, idempotencyErr
		}
		return document, true, nil
	}
	return Document{}, false, ErrConflict
}

func (service *Service) invoiceLines(facts OrderFacts) []Line {
	lines := make([]Line, 0, len(facts.Items)+1)
	for _, item := range facts.Items {
		lines = append(lines, Line{
			Name:           item.Name,
			Configuration:  singleLine(item.Configuration),
			Quantity:       item.Quantity,
			UnitPriceMinor: item.UnitPriceMinor,
			TotalMinor:     item.LineTotalMinor,
		})
	}
	if facts.ShippingTotalMinor != 0 {
		name := service.shippingLabel
		if facts.HasShippingMethod {
			if method := strings.TrimSpace(facts.ShippingMethodName); method != "" {
				name = method
			}
		}
		lines = append(lines, Line{
			Name:           name,
			Quantity:       1,
			UnitPriceMinor: facts.ShippingTotalMinor,
			TotalMinor:     facts.ShippingTotalMinor,
		})
	}
	return lines
}

func (service *Service) sellerParty() Party {
	return Party{
		Name:    service.seller.Name,
		ID:      service.seller.ID,
		Address: service.seller.Address,
		Email:   service.seller.Email,
		Phone:   service.seller.Phone,
	}
}

func buyerFromFacts(facts OrderFacts) (Party, string) {
	address := facts.BillingAddress
	name := strings.TrimSpace(address.FirstName + " " + address.LastName)
	if company := strings.TrimSpace(address.Company); company != "" {
		name = company
	}
	fields := []string{address.Street, address.City, address.PostalCode}
	parts := make([]string, 0, len(fields)+1)
	for _, part := range fields {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	country := strings.ToUpper(strings.TrimSpace(address.CountryCode))
	if country != "" {
		parts = append(parts, country)
	}
	buyer := Party{
		Name: name, Address: strings.Join(parts, ", "), Email: facts.ContactEmail,
	}
	return buyer, country
}

func deliveryCountry(facts OrderFacts) string {
	return strings.ToUpper(strings.TrimSpace(facts.ShippingAddress.CountryCode))
}
