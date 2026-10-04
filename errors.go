package invoiceledger

import "errors"

// The sentinel error set returned by Service and by Store implementations.
var (
	// ErrUnprocessable reports a request or a fact set that violates the
	// ledger contract.
	ErrUnprocessable = errors.New("invoiceledger: request cannot be processed")

	// ErrNotFound reports a missing order or a missing document.
	ErrNotFound = errors.New("invoiceledger: document was not found")

	// ErrConflict reports ledger state that conflicts with the request.
	ErrConflict = errors.New("invoiceledger: state conflicts with the request")

	// ErrIdempotencyConflict reports a reused idempotency key whose payload
	// differs from the stored payload.
	ErrIdempotencyConflict = errors.New("invoiceledger: idempotency key conflicts with the request")

	// ErrLegacyTax reports an order that carries tax data the ledger cannot
	// invoice.
	ErrLegacyTax = errors.New("invoiceledger: legacy taxed order cannot be invoiced")

	// ErrMisconfigured reports a New call with a missing or an invalid
	// option, for example a nil Store or Source.
	ErrMisconfigured = errors.New("invoiceledger: service is misconfigured")

	// ErrUnavailable reports a failing Store or Source.
	ErrUnavailable = errors.New("invoiceledger: store is unavailable")

	// ErrNoRenderer reports a Render call without a configured Renderer.
	ErrNoRenderer = errors.New("invoiceledger: no renderer is configured")
)
