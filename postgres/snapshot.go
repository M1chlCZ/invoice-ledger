package postgres

import (
	"encoding/json"
	"fmt"

	"github.com/m1chlcz/invoice-ledger"
)

// snapshotRecord is the flat snapshot shape the shop schema stores in
// shop.invoices.snapshot. It keeps readers of the shop column working.
type snapshotRecord struct {
	Number         string               `json:"number"`
	Kind           string               `json:"kind"`
	OriginalNumber string               `json:"original_number,omitempty"`
	IssuedDate     string               `json:"issued_date"`
	SupplyDate     string               `json:"supply_date,omitempty"`
	OrderNumber    string               `json:"order_number"`
	Locale         string               `json:"locale"`
	Currency       string               `json:"currency"`
	SellerName     string               `json:"seller_name"`
	SellerID       string               `json:"seller_id"`
	SellerAddress  string               `json:"seller_address"`
	SellerEmail    string               `json:"seller_email"`
	SellerPhone    string               `json:"seller_phone"`
	BuyerName      string               `json:"buyer_name"`
	BuyerAddress   string               `json:"buyer_address"`
	BuyerEmail     string               `json:"buyer_email"`
	Lines          []invoiceledger.Line `json:"lines"`
	ItemsMinor     int64                `json:"items_minor"`
	ShippingMinor  int64                `json:"shipping_minor"`
	DiscountMinor  int64                `json:"discount_minor"`
	TotalMinor     int64                `json:"total_minor"`
	Reason         string               `json:"reason,omitempty"`
	Paid           bool                 `json:"paid"`
}

func encodeSnapshot(snapshot invoiceledger.Snapshot) ([]byte, error) {
	record := snapshotRecord{
		Number:         snapshot.Number,
		Kind:           string(snapshot.Kind),
		OriginalNumber: snapshot.OriginalNumber,
		IssuedDate:     snapshot.IssuedDate,
		SupplyDate:     snapshot.SupplyDate,
		OrderNumber:    snapshot.OrderNumber,
		Locale:         snapshot.Locale,
		Currency:       snapshot.Currency,
		SellerName:     snapshot.Seller.Name,
		SellerID:       snapshot.Seller.ID,
		SellerAddress:  snapshot.Seller.Address,
		SellerEmail:    snapshot.Seller.Email,
		SellerPhone:    snapshot.Seller.Phone,
		BuyerName:      snapshot.Buyer.Name,
		BuyerAddress:   snapshot.Buyer.Address,
		BuyerEmail:     snapshot.Buyer.Email,
		Lines:          snapshot.Lines,
		ItemsMinor:     snapshot.ItemsMinor,
		ShippingMinor:  snapshot.ShippingMinor,
		DiscountMinor:  snapshot.DiscountMinor,
		TotalMinor:     snapshot.TotalMinor,
		Reason:         snapshot.Reason,
		Paid:           snapshot.Paid,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf(
			"invoiceledger/postgres: encode snapshot: %w: %w",
			invoiceledger.ErrUnavailable, err,
		)
	}
	return encoded, nil
}

func decodeSnapshot(raw []byte) (invoiceledger.Snapshot, error) {
	if len(raw) == 0 {
		return invoiceledger.Snapshot{}, nil
	}
	var record snapshotRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return invoiceledger.Snapshot{}, fmt.Errorf(
			"invoiceledger/postgres: decode snapshot: %w: %w",
			invoiceledger.ErrUnavailable, err,
		)
	}
	return invoiceledger.Snapshot{
		Number:         record.Number,
		Kind:           invoiceledger.Kind(record.Kind),
		OriginalNumber: record.OriginalNumber,
		IssuedDate:     record.IssuedDate,
		SupplyDate:     record.SupplyDate,
		OrderNumber:    record.OrderNumber,
		Locale:         record.Locale,
		Currency:       record.Currency,
		Seller: invoiceledger.Party{
			Name:    record.SellerName,
			ID:      record.SellerID,
			Address: record.SellerAddress,
			Email:   record.SellerEmail,
			Phone:   record.SellerPhone,
		},
		Buyer: invoiceledger.Party{
			Name:    record.BuyerName,
			Address: record.BuyerAddress,
			Email:   record.BuyerEmail,
		},
		Lines:         record.Lines,
		ItemsMinor:    record.ItemsMinor,
		ShippingMinor: record.ShippingMinor,
		DiscountMinor: record.DiscountMinor,
		TotalMinor:    record.TotalMinor,
		Reason:        record.Reason,
		Paid:          record.Paid,
	}, nil
}
