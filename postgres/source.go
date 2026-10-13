package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/m1chlcz/invoice-ledger"
)

const (
	orderStatusCancelled = "cancelled"
	paymentStatusPaid    = "paid"
)

const orderSelect = `
	SELECT id, order_number, contact_email, email_locale, currency,
	       billing_address, shipping_address,
	       items_total_minor, shipping_total_minor, discount_total_minor,
	       grand_total_minor, order_status, payment_status
	FROM shop.orders
	WHERE id = $1`

const orderItemsSelect = `
	SELECT name, customization, quantity, unit_price_minor, line_total_minor,
	       tax_rate_basis_points, tax_minor
	FROM shop.order_items
	WHERE order_id = $1
	ORDER BY created_at, id`

const orderShippingSelect = `
	SELECT method_name, tax_rate_basis_points, tax_minor
	FROM shop.order_shipping
	WHERE order_id = $1`

type orderAddress struct {
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
	Company     string `json:"company,omitempty"`
	Street      string `json:"street"`
	City        string `json:"city"`
	PostalCode  string `json:"postal_code"`
	CountryCode string `json:"country_code"`
}

// Order implements invoiceledger.Source. It reads the order, its items and
// its shipping row from the shop schema and maps them onto OrderFacts.
func (store *Store) Order(ctx context.Context, orderID uuid.UUID) (invoiceledger.OrderFacts, error) {
	var (
		facts           invoiceledger.OrderFacts
		billingAddress  []byte
		shippingAddress []byte
		orderStatus     string
		paymentStatus   string
	)
	err := store.pool.QueryRow(ctx, orderSelect, orderID).Scan(
		&facts.OrderID, &facts.OrderNumber, &facts.ContactEmail, &facts.Locale,
		&facts.Currency, &billingAddress, &shippingAddress,
		&facts.ItemsTotalMinor, &facts.ShippingTotalMinor,
		&facts.DiscountTotalMinor, &facts.GrandTotalMinor,
		&orderStatus, &paymentStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return invoiceledger.OrderFacts{}, invoiceledger.ErrNotFound
	}
	if err != nil {
		return invoiceledger.OrderFacts{}, translate("read order", err)
	}
	facts.BillingAddress, err = decodeAddress(billingAddress)
	if err != nil {
		return invoiceledger.OrderFacts{}, err
	}
	facts.ShippingAddress, err = decodeAddress(shippingAddress)
	if err != nil {
		return invoiceledger.OrderFacts{}, err
	}
	items, err := store.orderItems(ctx, orderID)
	if err != nil {
		return invoiceledger.OrderFacts{}, err
	}
	facts.Items = items
	hasMethod, methodName, shippingRate, shippingTax, err := store.orderShipping(ctx, orderID)
	if err != nil {
		return invoiceledger.OrderFacts{}, err
	}
	facts.HasShippingMethod = hasMethod
	facts.ShippingMethodName = methodName
	facts.ShippingTaxRateBasisPoints = shippingRate
	facts.ShippingTaxMinor = shippingTax
	facts.Cancelled = orderStatus == orderStatusCancelled
	facts.Paid = paymentStatus == paymentStatusPaid
	return facts, nil
}

func (store *Store) orderItems(
	ctx context.Context,
	orderID uuid.UUID,
) ([]invoiceledger.OrderItem, error) {
	rows, err := store.pool.Query(ctx, orderItemsSelect, orderID)
	if err != nil {
		return nil, translate("read order items", err)
	}
	defer rows.Close()
	items := make([]invoiceledger.OrderItem, 0)
	for rows.Next() {
		var (
			item          invoiceledger.OrderItem
			customization []byte
			quantity      int32
		)
		if scanErr := rows.Scan(
			&item.Name, &customization, &quantity, &item.UnitPriceMinor,
			&item.LineTotalMinor, &item.TaxRateBasisPoints, &item.TaxMinor,
		); scanErr != nil {
			return nil, translate("read order items", scanErr)
		}
		item.Quantity = int64(quantity)
		item.Configuration = configurationSummary(customization)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, translate("read order items", err)
	}
	return items, nil
}

func (store *Store) orderShipping(
	ctx context.Context,
	orderID uuid.UUID,
) (bool, string, int32, int64, error) {
	var (
		methodName *string
		rate       *int32
		tax        *int64
	)
	err := store.pool.QueryRow(ctx, orderShippingSelect, orderID).Scan(&methodName, &rate, &tax)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", 0, 0, nil
	}
	if err != nil {
		return false, "", 0, 0, translate("read order shipping", err)
	}
	var (
		name      string
		rateValue int32
		taxValue  int64
	)
	if methodName != nil {
		name = strings.TrimSpace(*methodName)
	}
	if rate != nil {
		rateValue = *rate
	}
	if tax != nil {
		taxValue = *tax
	}
	return true, name, rateValue, taxValue, nil
}

func decodeAddress(raw []byte) (invoiceledger.Address, error) {
	if len(raw) == 0 {
		return invoiceledger.Address{}, nil
	}
	var address orderAddress
	if err := json.Unmarshal(raw, &address); err != nil {
		return invoiceledger.Address{}, fmt.Errorf(
			"invoiceledger/postgres: decode order address: %w: %w",
			invoiceledger.ErrUnavailable, err,
		)
	}
	return invoiceledger.Address{
		FirstName:   address.FirstName,
		LastName:    address.LastName,
		Company:     address.Company,
		Street:      address.Street,
		City:        address.City,
		PostalCode:  address.PostalCode,
		CountryCode: address.CountryCode,
	}, nil
}

func configurationSummary(raw []byte) string {
	var payload struct {
		Description string `json:"description"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	return strings.Join(strings.Fields(payload.Description), " ")
}
