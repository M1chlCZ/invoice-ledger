package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/m1chlcz/invoice-ledger"
)

func writeInputFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "documents.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write input file: %v", err)
	}
	return path
}

func testDocument() invoiceledger.Document {
	return invoiceledger.Document{
		ID:             uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		SequenceYear:   2026,
		SequenceNumber: 1,
		Number:         "FA-2026-000001",
		Kind:           invoiceledger.KindInvoice,
		OrderNumber:    "1001",
		Currency:       "CZK",
		ItemsMinor:     1000,
		TotalMinor:     1000,
		IssuedDate:     "2026-03-15",
		Paid:           true,
		Locale:         "cs-CZ",
	}
}

func failingFetcher(t *testing.T) documentFetcher {
	t.Helper()
	return func(context.Context, string) ([]invoiceledger.Document, error) {
		t.Fatal("database fetch must not run for an offline export")
		return nil, nil
	}
}

func TestRunRejectsUsageErrors(t *testing.T) {
	t.Setenv("INVOICELEDGER_DATABASE_URL", "")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "no command", args: nil, want: "usage:"},
		{name: "unknown command", args: []string{"frobnicate"}, want: "unknown command"},
		{
			name: "missing source",
			args: []string{"export-csv"},
			want: "is required",
		},
		{
			name: "both sources",
			args: []string{"export-csv", "-database-url", "postgres://example", "-input", "in.json"},
			want: "mutually exclusive",
		},
		{
			name: "unexpected arguments",
			args: []string{"export-csv", "-input", "in.json", "extra"},
			want: "unexpected arguments",
		},
		{
			name: "unknown flag",
			args: []string{"export-csv", "-nope"},
			want: "flag provided but not defined",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runWith(testCase.args, &stdout, &stderr, failingFetcher(t))
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), testCase.want) {
				t.Fatalf("stderr %q does not contain %q", stderr.String(), testCase.want)
			}
		})
	}
}

func TestRunOfflineExportToStdout(t *testing.T) {
	inputPath := writeInputFile(t, `[
		{
			"id": "11111111-1111-1111-1111-111111111111",
			"sequence_year": 2026,
			"sequence_number": 1,
			"number": "FA-2026-000001",
			"kind": "invoice",
			"order_number": "1001",
			"currency": "CZK",
			"items_minor": 1000,
			"total_minor": 1000,
			"issued_date": "2026-03-15",
			"paid": true,
			"locale": "cs-CZ"
		}
	]`)
	var stdout, stderr bytes.Buffer
	code := runWith(
		[]string{"export-csv", "-input", inputPath},
		&stdout, &stderr, failingFetcher(t),
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	var expected bytes.Buffer
	if err := invoiceledger.ExportCSV(&expected, []invoiceledger.Document{testDocument()}); err != nil {
		t.Fatalf("reference export: %v", err)
	}
	if stdout.String() != expected.String() {
		t.Fatalf("stdout = %q, want %q", stdout.String(), expected.String())
	}
}

func TestRunOfflineExportToFile(t *testing.T) {
	inputPath := writeInputFile(t, `[]`)
	outputPath := filepath.Join(t.TempDir(), "export.csv")
	var stdout, stderr bytes.Buffer
	code := runWith(
		[]string{"export-csv", "-input", inputPath, "-output", outputPath},
		&stdout, &stderr, failingFetcher(t),
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var expected bytes.Buffer
	if exportErr := invoiceledger.ExportCSV(&expected, nil); exportErr != nil {
		t.Fatalf("reference export: %v", exportErr)
	}
	if string(content) != expected.String() {
		t.Fatalf("output file = %q, want %q", content, expected.String())
	}
}

func TestRunReportsInputFailures(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "invalid json", content: "not json"},
		{name: "trailing content", content: "[] []"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			inputPath := writeInputFile(t, testCase.content)
			var stdout, stderr bytes.Buffer
			code := runWith(
				[]string{"export-csv", "-input", inputPath},
				&stdout, &stderr, failingFetcher(t),
			)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "decode input") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runWith(
			[]string{"export-csv", "-input", filepath.Join(t.TempDir(), "missing.json")},
			&stdout, &stderr, failingFetcher(t),
		)
		if code != 1 || !strings.Contains(stderr.String(), "open input") {
			t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
		}
	})
	t.Run("unwritable output", func(t *testing.T) {
		inputPath := writeInputFile(t, "[]")
		var stdout, stderr bytes.Buffer
		code := runWith(
			[]string{
				"export-csv", "-input", inputPath,
				"-output", filepath.Join(t.TempDir(), "missing", "export.csv"),
			},
			&stdout, &stderr, failingFetcher(t),
		)
		if code != 1 || !strings.Contains(stderr.String(), "open output") {
			t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
		}
	})
}

func TestRunOfflineExportIgnoresEnvironmentDatabase(t *testing.T) {
	t.Setenv("INVOICELEDGER_DATABASE_URL", "postgres://example/env")
	inputPath := writeInputFile(t, "[]")
	var stdout, stderr bytes.Buffer
	code := runWith(
		[]string{"export-csv", "-input", inputPath},
		&stdout, &stderr, failingFetcher(t),
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
}

func TestRunDatabaseExport(t *testing.T) {
	var requestedURL string
	fetch := func(_ context.Context, databaseURL string) ([]invoiceledger.Document, error) {
		requestedURL = databaseURL
		return []invoiceledger.Document{testDocument()}, nil
	}
	var stdout, stderr bytes.Buffer
	code := runWith(
		[]string{"export-csv", "-database-url", "postgres://example/ledger"},
		&stdout, &stderr, fetch,
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if requestedURL != "postgres://example/ledger" {
		t.Fatalf("database url = %q", requestedURL)
	}
	var expected bytes.Buffer
	if err := invoiceledger.ExportCSV(&expected, []invoiceledger.Document{testDocument()}); err != nil {
		t.Fatalf("reference export: %v", err)
	}
	if stdout.String() != expected.String() {
		t.Fatalf("stdout = %q, want %q", stdout.String(), expected.String())
	}
}

func TestRunDatabaseEnvironmentFallbackAndFailure(t *testing.T) {
	t.Setenv("INVOICELEDGER_DATABASE_URL", "postgres://example/env")
	fetched := false
	fetch := func(_ context.Context, databaseURL string) ([]invoiceledger.Document, error) {
		fetched = true
		if databaseURL != "postgres://example/env" {
			t.Fatalf("database url = %q", databaseURL)
		}
		return nil, errors.New("database down")
	}
	var stdout, stderr bytes.Buffer
	code := runWith([]string{"export-csv"}, &stdout, &stderr, fetch)
	if code != 1 || !fetched {
		t.Fatalf("exit code = %d fetched=%t", code, fetched)
	}
	if !strings.Contains(stderr.String(), "database down") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunReportsInvalidDatabaseURL(t *testing.T) {
	t.Setenv("INVOICELEDGER_DATABASE_URL", "")
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"export-csv", "-database-url", "://bad"},
		&stdout, &stderr,
	)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "parse database URL") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
