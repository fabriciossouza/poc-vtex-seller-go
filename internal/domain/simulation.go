package domain

import "time"

type QuotedItem struct {
	Item
	UnitPriceCents int64
	StockOnHand    int
	Available      bool
}

type ShippingSLA struct {
	PriceCents int64
	Estimate   time.Duration
}

type Simulation struct {
	Items       []QuotedItem
	ShippingSLA ShippingSLA
}

func QuoteItem(item Item, unitPriceCents int64, stockOnHand int) QuotedItem {
	return QuoteItemForDemand(item, unitPriceCents, stockOnHand, item.Quantity)
}

// QuoteItemForDemand compares the stock with the total quantity the cart asks
// for of this SKU, which exceeds the item's own when the SKU repeats.
func QuoteItemForDemand(item Item, unitPriceCents int64, stockOnHand, demand int) QuotedItem {
	return QuotedItem{
		Item:           item,
		UnitPriceCents: unitPriceCents,
		StockOnHand:    stockOnHand,
		Available:      stockOnHand >= demand,
	}
}

func (s Simulation) Purchasable() bool {
	for _, it := range s.Items {
		if !it.Available {
			return false
		}
	}
	return true
}

// ItemsTotalCents ignores unavailable items: the marketplace must not be
// charged for what the seller cannot deliver.
func (s Simulation) ItemsTotalCents() int64 {
	var total int64
	for _, it := range s.Items {
		if it.Available {
			total += it.UnitPriceCents * int64(it.Quantity)
		}
	}
	return total
}
