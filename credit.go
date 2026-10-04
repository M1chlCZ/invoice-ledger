package invoiceledger

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// CreditRequest records confirmed external refund evidence as an append-only
// credit note linked to the original invoice.
type CreditRequest struct {
	InvoiceID       uuid.UUID
	AmountMinor     int64
	Reason          string
	RefundReference string
	RefundedOn      string
	IdempotencyKey  uuid.UUID
}

// Credit creates a credit note that negates part or all of one original
// invoice. Credit is idempotent by key and payload hash and is safe to replay.
// Calls on one Service are serialized, so the uncredited remainder cannot be
// exceeded inside one process. When the configured Store implements
// CreditInserter, the insert itself enforces the limit atomically; otherwise a
// shared cross-process Store must enforce the same rule itself.
//
// The amount must be positive and cannot exceed the uncredited remainder.
// The reason, the refund reference, and the refunded date are required. The
// original document must exist and must be an invoice. A repeated key with
// the same payload returns the stored document with replayed true; the same
// key with a different payload returns ErrIdempotencyConflict.
func (service *Service) Credit(ctx context.Context, request CreditRequest) (Document, bool, error) {
	if request.AmountMinor <= 0 ||
		!validText(request.Reason, reasonMaxRunes, true) ||
		!validText(request.RefundReference, refundReferenceMaxRunes, true) ||
		request.RefundedOn == "" || !validDate(request.RefundedOn) {
		return Document{}, false, ErrUnprocessable
	}
	now := service.clock()
	hash := payloadHash(
		"correction", request.InvoiceID.String(),
		strconv.FormatInt(request.AmountMinor, 10), request.Reason,
		request.RefundReference, request.RefundedOn,
	)
	service.mutex.Lock()
	defer service.mutex.Unlock()
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
	original, err := service.store.ByID(ctx, request.InvoiceID)
	if err != nil {
		return Document{}, false, err
	}
	if original.Kind != KindInvoice {
		return Document{}, false, ErrConflict
	}
	credited, err := service.store.CreditedTotal(ctx, original.ID)
	if err != nil {
		return Document{}, false, err
	}
	if credited > 0 {
		return Document{}, false, ErrUnavailable
	}
	remaining, ok := remainingCredit(original.TotalMinor, credited)
	if !ok || request.AmountMinor > remaining {
		return Document{}, false, ErrConflict
	}
	document, err := service.buildCredit(ctx, original, request, hash, now)
	if err != nil {
		return Document{}, false, err
	}
	stored, err := service.insertCredit(ctx, document, original)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return service.replayCredit(ctx, request, hash)
		}
		return Document{}, false, err
	}
	return stored, false, nil
}

// insertCredit prefers the atomic CreditInserter capability and falls back to
// the plain check-then-insert path of the base Store.
func (service *Service) insertCredit(
	ctx context.Context,
	document Document,
	original Document,
) (Document, error) {
	if inserter, ok := service.store.(CreditInserter); ok {
		return inserter.InsertCredit(ctx, document, original.ID, original.TotalMinor)
	}
	return service.store.Insert(ctx, document)
}

func (service *Service) buildCredit(
	ctx context.Context,
	original Document,
	request CreditRequest,
	hash []byte,
	now time.Time,
) (Document, error) {
	issued := now.In(service.location)
	sequence, err := service.store.AllocateNumber(ctx, issued.Year(), KindCreditNote)
	if err != nil {
		return Document{}, err
	}
	number := sequence.Format(service.creditPrefix)
	issuedDate := issued.Format(dateLayout)
	negative := -request.AmountMinor
	snapshot := Snapshot{
		Number:         number,
		Kind:           KindCreditNote,
		OriginalNumber: original.Number,
		IssuedDate:     issuedDate,
		SupplyDate:     request.RefundedOn,
		OrderNumber:    original.Snapshot.OrderNumber,
		Locale:         original.Locale,
		Currency:       original.Currency,
		Seller:         original.Snapshot.Seller,
		Buyer:          original.Snapshot.Buyer,
		Lines: []Line{{
			Name:           service.creditNoteLabel + original.Number,
			Configuration:  request.Reason,
			Quantity:       1,
			UnitPriceMinor: negative,
			TotalMinor:     negative,
		}},
		ItemsMinor:    negative,
		ShippingMinor: 0,
		DiscountMinor: 0,
		TotalMinor:    negative,
		Reason:        request.Reason,
		Paid:          original.Paid,
	}
	originalID := original.ID
	document := Document{
		ID:                     uuid.New(),
		SequenceYear:           sequence.Year,
		SequenceNumber:         sequence.Number,
		Number:                 number,
		Kind:                   KindCreditNote,
		OrderNumber:            original.OrderNumber,
		OriginalID:             &originalID,
		OriginalNumber:         original.Number,
		RefundReference:        request.RefundReference,
		RefundedOn:             request.RefundedOn,
		Currency:               original.Currency,
		ItemsMinor:             negative,
		ShippingMinor:          0,
		DiscountMinor:          0,
		TotalMinor:             negative,
		SupplyDate:             request.RefundedOn,
		IssuedDate:             issuedDate,
		IssuedAt:               normalizeTime(now),
		Paid:                   original.Paid,
		Reason:                 request.Reason,
		Locale:                 original.Locale,
		BuyerCountry:           original.BuyerCountry,
		Snapshot:               snapshot,
		CreatedAt:              normalizeTime(now),
		IdempotencyKey:         optionalUUID(request.IdempotencyKey),
		IdempotencyPayloadHash: hash,
	}
	return document, nil
}

func (service *Service) replayCredit(
	ctx context.Context,
	request CreditRequest,
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
	return Document{}, false, ErrConflict
}

func remainingCredit(original, credited int64) (int64, bool) {
	remaining := original + credited
	switch {
	case credited > 0 && remaining < original:
		return 0, false
	case credited < 0 && remaining > original:
		return 0, false
	default:
		return remaining, true
	}
}
