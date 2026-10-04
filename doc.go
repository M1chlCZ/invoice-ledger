// Package invoiceledger is a pure-domain, append-only invoice ledger.
//
// The package issues and reads immutable invoice and credit-note documents.
// It contains no database, network, HTTP, or rendering code. The caller
// supplies order facts through a Source, persistence through a Store, and an
// optional document renderer through a Renderer.
package invoiceledger
