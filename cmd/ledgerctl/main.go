// Command ledgerctl exports the invoice ledger.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/m1chlcz/invoice-ledger"
	"github.com/m1chlcz/invoice-ledger/postgres"
)

const usageText = `usage: ledgerctl export-csv [flags]

commands:
  export-csv  write ledger documents as CSV

flags:
  -database-url  PostgreSQL connection string (default INVOICELEDGER_DATABASE_URL)
  -input         JSON file with an array of documents (offline export)
  -output        output file (default stdout)

exit codes:
  0  success
  1  runtime failure
  2  usage error
`

type documentFetcher func(ctx context.Context, databaseURL string) ([]invoiceledger.Document, error)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWith(args, stdout, stderr, fetchDocuments)
}

// runWith is the testable CLI logic. fetch supplies the database export so
// tests can replace it without a running PostgreSQL server.
func runWith(args []string, stdout, stderr io.Writer, fetch documentFetcher) int {
	if len(args) == 0 || args[0] != "export-csv" {
		if len(args) != 0 {
			fmt.Fprintf(stderr, "ledgerctl: unknown command %q\n", args[0])
		}
		fmt.Fprint(stderr, usageText)
		return 2
	}
	flags := flag.NewFlagSet("ledgerctl export-csv", flag.ContinueOnError)
	flags.SetOutput(stderr)
	databaseURL := flags.String(
		"database-url", os.Getenv("INVOICELEDGER_DATABASE_URL"),
		"PostgreSQL connection string",
	)
	input := flags.String("input", "", "JSON file with an array of documents")
	output := flags.String("output", "", "output file (default stdout)")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "ledgerctl: unexpected arguments")
		return 2
	}
	databaseProvided := false
	flags.Visit(func(visited *flag.Flag) {
		if visited.Name == "database-url" {
			databaseProvided = true
		}
	})
	if *input != "" && databaseProvided {
		fmt.Fprintln(stderr, "ledgerctl: -database-url and -input are mutually exclusive")
		return 2
	}
	if *input == "" && *databaseURL == "" {
		fmt.Fprintln(stderr, "ledgerctl: -database-url, INVOICELEDGER_DATABASE_URL or -input is required")
		return 2
	}
	ctx := context.Background()
	documents, err := loadDocuments(ctx, *databaseURL, *input, fetch)
	if err != nil {
		fmt.Fprintf(stderr, "ledgerctl: %v\n", err)
		return 1
	}
	writer, closeWriter, err := openOutput(*output, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "ledgerctl: %v\n", err)
		return 1
	}
	if exportErr := invoiceledger.ExportCSV(writer, documents); exportErr != nil {
		_ = closeWriter()
		fmt.Fprintf(stderr, "ledgerctl: %v\n", exportErr)
		return 1
	}
	if closeErr := closeWriter(); closeErr != nil {
		fmt.Fprintf(stderr, "ledgerctl: %v\n", closeErr)
		return 1
	}
	return 0
}

func loadDocuments(
	ctx context.Context,
	databaseURL, input string,
	fetch documentFetcher,
) ([]invoiceledger.Document, error) {
	if input != "" {
		return readDocumentsFile(input)
	}
	return fetch(ctx, databaseURL)
}

func readDocumentsFile(path string) ([]invoiceledger.Document, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open input %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(file)
	var documents []invoiceledger.Document
	if err := decoder.Decode(&documents); err != nil {
		return nil, fmt.Errorf("decode input %s: %w", path, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode input %s: unexpected trailing content", path)
	}
	return documents, nil
}

func openOutput(path string, stdout io.Writer) (io.Writer, func() error, error) {
	if path == "" || path == "-" {
		return stdout, func() error { return nil }, nil
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open output %s: %w", path, err)
	}
	return file, file.Close, nil
}

func fetchDocuments(ctx context.Context, databaseURL string) ([]invoiceledger.Document, error) {
	store, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.ListDocuments(ctx)
}
