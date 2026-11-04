package domain_test

import (
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

func TestQuoteItemDeniesWhenStockIsBelowRequested(t *testing.T) {
	q := domain.QuoteItem(domain.Item{SKU: "a", Quantity: 3}, 1000, 2)
	if q.Available {
		t.Fatal("item with insufficient stock must not be available")
	}
	if q.StockOnHand != 2 {
		t.Fatalf("StockOnHand = %d", q.StockOnHand)
	}
}

func TestQuoteItemAcceptsWhenStockCoversQuantity(t *testing.T) {
	q := domain.QuoteItem(domain.Item{SKU: "a", Quantity: 2}, 1000, 2)
	if !q.Available || q.UnitPriceCents != 1000 {
		t.Fatalf("got %+v", q)
	}
}

func TestSimulationTotalsOnlyAvailableItemsAndIsPurchasableOnlyIfAllAre(t *testing.T) {
	sim := domain.Simulation{
		Items: []domain.QuotedItem{
			domain.QuoteItem(domain.Item{SKU: "a", Quantity: 2}, 1000, 5),
			domain.QuoteItem(domain.Item{SKU: "b", Quantity: 1}, 500, 0),
		},
		ShippingSLA: domain.ShippingSLA{PriceCents: 700, Estimate: 48 * time.Hour},
	}
	if sim.Purchasable() {
		t.Fatal("simulation with an out-of-stock item must not be purchasable")
	}
	if got := sim.ItemsTotalCents(); got != 2000 {
		t.Fatalf("ItemsTotalCents = %d, want 2000", got)
	}
}
