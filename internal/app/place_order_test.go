package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

var testNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func TestPlaceOrderIsIdempotentByKey(t *testing.T) {
	repo := newMemRepo()
	uc := app.NewPlaceOrder(repo, fixedClock{testNow})
	cmd := app.PlaceOrderCommand{
		OrderID:        "ord-1",
		IdempotencyKey: "key-1",
		Items:          []domain.Item{{SKU: "a", Quantity: 1}},
	}

	first, err := uc.Execute(context.Background(), cmd)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	cmd.OrderID = "ord-2" // retry from the marketplace must not create a second order
	second, err := uc.Execute(context.Background(), cmd)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if second.ID != first.ID || second.ID != "ord-1" {
		t.Fatalf("second.ID = %s, want ord-1", second.ID)
	}
	if len(repo.byID) != 1 || repo.saves != 1 {
		t.Fatalf("orders = %d, saves = %d; want 1 and 1", len(repo.byID), repo.saves)
	}
	if first.Status != domain.StatusReceived || !first.CreatedAt.Equal(testNow) {
		t.Fatalf("order = %+v", first)
	}
}

func TestPlaceOrderRejectsInvalidOrder(t *testing.T) {
	uc := app.NewPlaceOrder(newMemRepo(), fixedClock{testNow})
	_, err := uc.Execute(context.Background(), app.PlaceOrderCommand{OrderID: "o", IdempotencyKey: "k"})
	if !errors.Is(err, domain.ErrInvalidOrder) {
		t.Fatalf("err = %v", err)
	}
}

func TestPlaceOrderKeepsMarketplaceEndpointAndItemPrice(t *testing.T) {
	repo := newMemRepo()
	uc := app.NewPlaceOrder(repo, fixedClock{testNow})

	order, err := uc.Execute(context.Background(), app.PlaceOrderCommand{
		OrderID:             "ord-1",
		IdempotencyKey:      "key-1",
		MarketplaceEndpoint: "https://marketplace.example/",
		Items:               []domain.Item{{SKU: "a", Quantity: 1, PriceCents: 2499}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	stored, _ := repo.Get(context.Background(), order.ID)
	if stored.MarketplaceEndpoint != "https://marketplace.example/" || stored.Items[0].PriceCents != 2499 {
		t.Fatalf("stored = %+v", stored)
	}
}
