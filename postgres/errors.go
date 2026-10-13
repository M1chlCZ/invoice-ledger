package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/m1chlcz/invoice-ledger"
)

const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeInvalidText         = "22P02"
	codeInvalidDatetime     = "22007"
	codeDatetimeOverflow    = "22008"
)

// translate maps driver and database failures onto the ledger sentinel
// errors. Everything the mapping does not classify becomes ErrUnavailable.
func translate(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return invoiceledger.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case codeUniqueViolation, codeForeignKeyViolation:
			return invoiceledger.ErrConflict
		case codeCheckViolation, codeInvalidText, codeInvalidDatetime, codeDatetimeOverflow:
			return invoiceledger.ErrUnprocessable
		}
	}
	return fmt.Errorf("invoiceledger/postgres: %s: %w: %w", operation, invoiceledger.ErrUnavailable, err)
}
