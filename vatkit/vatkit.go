// Package vatkit provides integer-only VAT and percentage arithmetic for
// gross-inclusive prices and integer minor units.
package vatkit

import (
	"errors"
	"math"
	"slices"
	"sort"
)

// BasisPoints is a rate in hundredths of a percent. The valid range is zero
// to 10000 inclusive; 2100 means 21.00 percent.
type BasisPoints int64

// Scale is the number of basis points in one whole unit.
const Scale BasisPoints = 10_000

// The sentinel error set returned by vatkit functions.
var (
	// ErrInvalidAmount reports a negative amount or a negative price.
	ErrInvalidAmount = errors.New("vatkit: amount is out of range")

	// ErrInvalidRate reports a rate outside zero to 10000.
	ErrInvalidRate = errors.New("vatkit: rate is out of range")

	// ErrInvalidQuantity reports a quantity outside the supported range.
	ErrInvalidQuantity = errors.New("vatkit: quantity is out of range")

	// ErrOverflow reports a checked arithmetic overflow.
	ErrOverflow = errors.New("vatkit: arithmetic overflow")
)

// EUCountryCodes returns a sorted copy of the European Union member-state
// codes. Mutating the returned slice does not affect IsEUCountry.
func EUCountryCodes() []string {
	codes := []string{
		"AT", "BE", "BG", "HR", "CY", "CZ", "DK",
		"EE", "FI", "FR", "DE", "GR", "HU", "IE",
		"IT", "LV", "LT", "LU", "MT", "NL", "PL",
		"PT", "RO", "SK", "SI", "ES", "SE",
	}
	sort.Strings(codes)
	return codes
}

// IsEUCountry reports whether code is a European Union member-state code.
// Codes are uppercase ISO 3166-1 alpha-2 codes; the match is exact and
// case-sensitive.
func IsEUCountry(code string) bool {
	return slices.Contains(EUCountryCodes(), code)
}

// TaxFromGross returns the tax part of a gross amount for a gross-inclusive
// rate. The result uses half-up rounding and stays within int64 for every
// valid input.
func TaxFromGross(gross int64, rate BasisPoints) (int64, error) {
	if gross < 0 {
		return 0, ErrInvalidAmount
	}
	if rate < 0 || rate > Scale {
		return 0, ErrInvalidRate
	}
	denominator := int64(Scale) + int64(rate)
	whole := gross / denominator * int64(rate)
	scaledRemainder := gross % denominator * int64(rate)
	tax := whole + scaledRemainder/denominator
	if 2*(scaledRemainder%denominator) >= denominator {
		tax++
	}
	return tax, nil
}

// ApplyBasisPoints returns the amount increased by a basis-point rate of the
// amount, rounded half-up. It is the inverse framing of a catalog price
// delta: the returned value is amount plus the computed delta.
func ApplyBasisPoints(amount int64, rate BasisPoints) (int64, error) {
	if amount < 0 {
		return 0, ErrInvalidAmount
	}
	if rate < 0 || rate > Scale {
		return 0, ErrInvalidRate
	}
	scale := int64(Scale)
	whole := amount / scale
	if rate != 0 && whole > math.MaxInt64/int64(rate) {
		return 0, ErrOverflow
	}
	delta := whole * int64(rate)
	scaledRemainder := amount % scale * int64(rate)
	roundedRemainder := scaledRemainder / scale
	if 2*(scaledRemainder%scale) >= scale {
		roundedRemainder++
	}
	delta, ok := addChecked(delta, roundedRemainder)
	if !ok {
		return 0, ErrOverflow
	}
	result, ok := addChecked(amount, delta)
	if !ok {
		return 0, ErrOverflow
	}
	return result, nil
}

// LineTotal returns the total of one line from a unit price and a quantity.
// The quantity must be positive and must fit an int32.
func LineTotal(unitPriceMinor, quantity int64) (int64, error) {
	if quantity <= 0 || quantity > math.MaxInt32 {
		return 0, ErrInvalidQuantity
	}
	if unitPriceMinor < 0 {
		return 0, ErrInvalidAmount
	}
	if unitPriceMinor != 0 && quantity > math.MaxInt64/unitPriceMinor {
		return 0, ErrOverflow
	}
	return unitPriceMinor * quantity, nil
}

// ShippingTotal returns the total shipping price for any non-negative number
// of shipments of the same unit price. A zero count returns zero.
func ShippingTotal(unitPriceMinor int64, shipmentCount int) (int64, error) {
	if unitPriceMinor < 0 {
		return 0, ErrInvalidAmount
	}
	if shipmentCount < 0 {
		return 0, ErrInvalidQuantity
	}
	if shipmentCount == 0 {
		return 0, nil
	}
	if unitPriceMinor > math.MaxInt64/int64(shipmentCount) {
		return 0, ErrOverflow
	}
	return unitPriceMinor * int64(shipmentCount), nil
}

// Accumulate adds one non-negative delta to a non-negative running total with
// an overflow check.
func Accumulate(current, delta int64) (int64, error) {
	if current < 0 || delta < 0 || current > math.MaxInt64-delta {
		return 0, ErrOverflow
	}
	return current + delta, nil
}

func addChecked(current, delta int64) (int64, bool) {
	if delta < 0 || current < 0 || current > math.MaxInt64-delta {
		return 0, false
	}
	return current + delta, true
}
