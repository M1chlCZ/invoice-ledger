package vatkit_test

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"sort"
	"testing"

	"github.com/m1chlcz/invoice-ledger/vatkit"
)

func TestTaxFromGrossUsesLineLevelHalfUpRounding(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		gross int64
		rate  vatkit.BasisPoints
		want  int64
	}{
		{name: "unit", gross: 1000, rate: 2100, want: 174},
		{name: "three units as one line", gross: 3000, rate: 2100, want: 521},
		{name: "shipping", gross: 990, rate: 2100, want: 172},
		{name: "zero rate", gross: math.MaxInt64, rate: 0, want: 0},
		{name: "odd denominator half rounds up", gross: 5001, rate: 1, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := vatkit.TaxFromGross(test.gross, test.rate)
			if err != nil {
				t.Fatalf("TaxFromGross: %v", err)
			}
			if got != test.want {
				t.Fatalf("TaxFromGross(%d, %d) = %d, want %d", test.gross, test.rate, got, test.want)
			}
		})
	}
}

func TestTaxFromGrossRejectsInvalidAndDoesNotOverflow(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		gross int64
		rate  vatkit.BasisPoints
	}{
		{gross: -1, rate: 2100},
		{gross: 1, rate: -1},
		{gross: 1, rate: 10_001},
	} {
		if _, err := vatkit.TaxFromGross(test.gross, test.rate); !errors.Is(err, vatkit.ErrInvalidAmount) &&
			!errors.Is(err, vatkit.ErrInvalidRate) {
			t.Fatalf("TaxFromGross(%d, %d) error = %v", test.gross, test.rate, err)
		}
	}
	if _, err := vatkit.TaxFromGross(math.MaxInt64, 9999); err != nil {
		t.Fatalf("overflow-safe maximum gross: %v", err)
	}
}

func TestTaxFromGrossMatchesBigReference(t *testing.T) {
	t.Parallel()

	rates := []vatkit.BasisPoints{0, 1, 500, 1200, 2100, 9900, 10000}
	random := rand.New(rand.NewSource(20260315))
	grosses := []int64{0, 1, 2, 999, 5000, 5001, math.MaxInt64 - 1, math.MaxInt64}
	for range 200 {
		grosses = append(grosses, random.Int63())
	}
	for _, gross := range grosses {
		for _, rate := range rates {
			got, err := vatkit.TaxFromGross(gross, rate)
			if err != nil {
				t.Fatalf("TaxFromGross(%d, %d): %v", gross, rate, err)
			}
			want := referenceTaxFromGross(gross, rate)
			if got != want {
				t.Fatalf("TaxFromGross(%d, %d) = %d, want %d", gross, rate, got, want)
			}
		}
	}
}

// referenceTaxFromGross computes round-half-up(gross*rate/(10000+rate)) with
// arbitrary-precision integers.
func referenceTaxFromGross(gross int64, rate vatkit.BasisPoints) int64 {
	denominator := big.NewInt(int64(rate) + 10000)
	numerator := new(big.Int).Mul(big.NewInt(gross), big.NewInt(int64(rate)))
	quotient := new(big.Int)
	remainder := new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if new(big.Int).Lsh(remainder, 1).Cmp(denominator) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient.Int64()
}

func TestApplyBasisPointsRoundsHalfUp(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		amount int64
		rate   vatkit.BasisPoints
		want   int64
	}{
		{amount: 10000, rate: 2100, want: 12100},
		{amount: 1000, rate: 2100, want: 1210},
		{amount: 500, rate: 0, want: 500},
		{amount: 0, rate: 10000, want: 0},
	} {
		got, err := vatkit.ApplyBasisPoints(test.amount, test.rate)
		if err != nil {
			t.Fatalf("ApplyBasisPoints(%d, %d): %v", test.amount, test.rate, err)
		}
		if got != test.want {
			t.Fatalf("ApplyBasisPoints(%d, %d) = %d, want %d", test.amount, test.rate, got, test.want)
		}
	}
}

func TestApplyBasisPointsRejectsInvalid(t *testing.T) {
	t.Parallel()

	if _, err := vatkit.ApplyBasisPoints(-1, 2100); !errors.Is(err, vatkit.ErrInvalidAmount) {
		t.Fatalf("negative amount error = %v", err)
	}
	if _, err := vatkit.ApplyBasisPoints(1, -1); !errors.Is(err, vatkit.ErrInvalidRate) {
		t.Fatalf("negative rate error = %v", err)
	}
	if _, err := vatkit.ApplyBasisPoints(1, 10_001); !errors.Is(err, vatkit.ErrInvalidRate) {
		t.Fatalf("rate above scale error = %v", err)
	}
}

func TestLineTotal(t *testing.T) {
	t.Parallel()

	total, err := vatkit.LineTotal(1000, 3)
	if err != nil || total != 3000 {
		t.Fatalf("LineTotal(1000, 3) = %d, %v", total, err)
	}
	if empty, emptyErr := vatkit.LineTotal(0, 5); emptyErr != nil || empty != 0 {
		t.Fatalf("LineTotal(0, 5) = %d, %v", empty, emptyErr)
	}
	if _, zeroErr := vatkit.LineTotal(1000, 0); !errors.Is(zeroErr, vatkit.ErrInvalidQuantity) {
		t.Fatalf("zero quantity error = %v", zeroErr)
	}
	if _, negativeErr := vatkit.LineTotal(1000, -1); !errors.Is(negativeErr, vatkit.ErrInvalidQuantity) {
		t.Fatalf("negative quantity error = %v", negativeErr)
	}
	if _, oversizedErr := vatkit.LineTotal(1000, math.MaxInt32+1); !errors.Is(oversizedErr, vatkit.ErrInvalidQuantity) {
		t.Fatalf("oversized quantity error = %v", oversizedErr)
	}
	if _, priceErr := vatkit.LineTotal(-1, 1); !errors.Is(priceErr, vatkit.ErrInvalidAmount) {
		t.Fatalf("negative price error = %v", priceErr)
	}
	if _, overflowErr := vatkit.LineTotal(math.MaxInt64, 2); !errors.Is(overflowErr, vatkit.ErrOverflow) {
		t.Fatalf("overflow error = %v", overflowErr)
	}
}

func TestShippingTotal(t *testing.T) {
	t.Parallel()

	total, err := vatkit.ShippingTotal(500, 2)
	if err != nil || total != 1000 {
		t.Fatalf("ShippingTotal(500, 2) = %d, %v", total, err)
	}
	if zero, zeroErr := vatkit.ShippingTotal(500, 0); zeroErr != nil || zero != 0 {
		t.Fatalf("ShippingTotal(500, 0) = %d, %v", zero, zeroErr)
	}
	if many, manyErr := vatkit.ShippingTotal(500, 3); manyErr != nil || many != 1500 {
		t.Fatalf("ShippingTotal(500, 3) = %d, %v", many, manyErr)
	}
	if _, negativeErr := vatkit.ShippingTotal(500, -1); !errors.Is(negativeErr, vatkit.ErrInvalidQuantity) {
		t.Fatalf("negative shipments error = %v", negativeErr)
	}
	if _, priceErr := vatkit.ShippingTotal(-1, 1); !errors.Is(priceErr, vatkit.ErrInvalidAmount) {
		t.Fatalf("negative price error = %v", priceErr)
	}
	if _, overflowErr := vatkit.ShippingTotal(math.MaxInt64, 2); !errors.Is(overflowErr, vatkit.ErrOverflow) {
		t.Fatalf("overflow error = %v", overflowErr)
	}
}

func TestAccumulate(t *testing.T) {
	t.Parallel()

	total, err := vatkit.Accumulate(1000, 1000)
	if err != nil || total != 2000 {
		t.Fatalf("Accumulate(1000, 1000) = %d, %v", total, err)
	}
	if _, overflowErr := vatkit.Accumulate(math.MaxInt64, 1); !errors.Is(overflowErr, vatkit.ErrOverflow) {
		t.Fatalf("overflow error = %v", overflowErr)
	}
	if _, totalErr := vatkit.Accumulate(-1, 1); !errors.Is(totalErr, vatkit.ErrOverflow) {
		t.Fatalf("negative total error = %v", totalErr)
	}
	if _, deltaErr := vatkit.Accumulate(1, -1); !errors.Is(deltaErr, vatkit.ErrOverflow) {
		t.Fatalf("negative delta error = %v", deltaErr)
	}
}

func TestIsEUCountry(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"AT", "CZ", "DE", "SK", "SE"} {
		if !vatkit.IsEUCountry(code) {
			t.Fatalf("IsEUCountry(%q) = false", code)
		}
	}
	for _, code := range []string{"", "US", "GB", "cz"} {
		if vatkit.IsEUCountry(code) {
			t.Fatalf("IsEUCountry(%q) = true", code)
		}
	}
}

func TestEUCountryCodesReturnsSortedCopy(t *testing.T) {
	t.Parallel()

	codes := vatkit.EUCountryCodes()
	if len(codes) != 27 {
		t.Fatalf("EUCountryCodes length = %d, want 27", len(codes))
	}
	if !sort.StringsAreSorted(codes) {
		t.Fatalf("EUCountryCodes is not sorted: %v", codes)
	}
	for index := range codes {
		codes[index] = "XX"
	}
	if !vatkit.IsEUCountry("CZ") || vatkit.IsEUCountry("XX") {
		t.Fatal("mutating EUCountryCodes changed the membership set")
	}
}
