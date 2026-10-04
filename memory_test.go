package invoiceledger_test

import (
	"testing"

	"github.com/m1chlcz/invoice-ledger"
	"github.com/m1chlcz/invoice-ledger/storetest"
)

func TestMemoryStoreConformance(t *testing.T) {
	t.Parallel()

	storetest.Run(t, func(t *testing.T) invoiceledger.Store {
		t.Helper()
		return invoiceledger.NewMemoryStore()
	})
}
