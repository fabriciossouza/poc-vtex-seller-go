package app_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

var testNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func TestPlaceOrderIsIdempotentByKey(t *testing.T) {
	repo := newMemRepo()
	uc := app.NewPlaceOrder(repo, fixedClock{testNow}, &fakeReserver{})
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
	uc := app.NewPlaceOrder(newMemRepo(), fixedClock{testNow}, &fakeReserver{})
	_, err := uc.Execute(context.Background(), app.PlaceOrderCommand{OrderID: "o", IdempotencyKey: "k"})
	if !errors.Is(err, domain.ErrInvalidOrder) {
		t.Fatalf("err = %v", err)
	}
}

func TestPlaceOrderKeepsMarketplaceEndpointAndItemPrice(t *testing.T) {
	repo := newMemRepo()
	uc := app.NewPlaceOrder(repo, fixedClock{testNow}, &fakeReserver{})

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

func placeCmd(items ...domain.Item) app.PlaceOrderCommand {
	return app.PlaceOrderCommand{OrderID: "ord-1", IdempotencyKey: "key-1", Items: items}
}

func TestPlaceOrderReservesStockForEachItemOfANewOrder(t *testing.T) {
	stock := &fakeReserver{}
	uc := app.NewPlaceOrder(newMemRepo(), fixedClock{testNow}, stock)

	_, err := uc.Execute(context.Background(), placeCmd(domain.Item{SKU: "a", Quantity: 2}, domain.Item{SKU: "b", Quantity: 1}))

	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := []string{"reserve a 2", "reserve b 1"}; !reflect.DeepEqual(stock.calls, want) {
		t.Fatalf("calls = %v, want %v", stock.calls, want)
	}
}

func TestPlaceOrderDoesNotReserveAgainForARepeatedKey(t *testing.T) {
	stock := &fakeReserver{}
	uc := app.NewPlaceOrder(newMemRepo(), fixedClock{testNow}, stock)
	cmd := placeCmd(domain.Item{SKU: "a", Quantity: 2})
	_, _ = uc.Execute(context.Background(), cmd)

	_, err := uc.Execute(context.Background(), cmd)

	if err != nil || len(stock.calls) != 1 {
		t.Fatalf("err = %v, calls = %v; want a single reservation", err, stock.calls)
	}
}

func TestPlaceOrderWithoutStockFailsReleasesWhatWasReservedAndStoresNothing(t *testing.T) {
	stock := &fakeReserver{failSKU: "b"}
	repo := newMemRepo()
	uc := app.NewPlaceOrder(repo, fixedClock{testNow}, stock)

	_, err := uc.Execute(context.Background(), placeCmd(domain.Item{SKU: "a", Quantity: 2}, domain.Item{SKU: "b", Quantity: 1}))

	if !errors.Is(err, ports.ErrInsufficientStock) {
		t.Fatalf("err = %v, want ErrInsufficientStock", err)
	}
	if want := []string{"reserve a 2", "reserve b 1", "release a 2"}; !reflect.DeepEqual(stock.calls, want) {
		t.Fatalf("calls = %v, want %v", stock.calls, want)
	}
	if len(repo.byID) != 0 {
		t.Fatalf("orders stored = %d", len(repo.byID))
	}
}

type failingRepo struct{ *memRepo }

func (failingRepo) Save(context.Context, *domain.Order) error { return errors.New("disk full") }

func TestPlaceOrderReleasesTheReservationWhenSavingFails(t *testing.T) {
	stock := &fakeReserver{}
	uc := app.NewPlaceOrder(failingRepo{newMemRepo()}, fixedClock{testNow}, stock)

	_, err := uc.Execute(context.Background(), placeCmd(domain.Item{SKU: "a", Quantity: 2}))

	if err == nil {
		t.Fatal("want the save error")
	}
	if want := []string{"reserve a 2", "release a 2"}; !reflect.DeepEqual(stock.calls, want) {
		t.Fatalf("calls = %v, want %v", stock.calls, want)
	}
}

// racedRepo mimics a concurrent placement of the same key winning the insert.
type racedRepo struct{ *memRepo }

func (racedRepo) Save(_ context.Context, o *domain.Order) error {
	*o = domain.Order{ID: "winner", IdempotencyKey: o.IdempotencyKey, Items: o.Items, Status: domain.StatusReceived}
	return nil
}

func TestPlaceOrderReleasesTheReservationWhenAConcurrentPlacementWon(t *testing.T) {
	stock := &fakeReserver{}
	uc := app.NewPlaceOrder(racedRepo{newMemRepo()}, fixedClock{testNow}, stock)

	order, err := uc.Execute(context.Background(), placeCmd(domain.Item{SKU: "a", Quantity: 2}))

	if err != nil || order.ID != "winner" {
		t.Fatalf("order = %+v, err = %v", order, err)
	}
	if want := []string{"reserve a 2", "release a 2"}; !reflect.DeepEqual(stock.calls, want) {
		t.Fatalf("calls = %v, want %v", stock.calls, want)
	}
}

// cancellingRepo cancels the caller's context while saving, as a client that
// hung up mid-request would, and then fails.
type cancellingRepo struct {
	*memRepo
	cancel context.CancelFunc
}

func (r cancellingRepo) Save(context.Context, *domain.Order) error {
	r.cancel()
	return errors.New("connection lost")
}

func TestPlaceOrderReleasesWithAContextThatSurvivesTheCallerHangingUp(t *testing.T) {
	stock := &fakeReserver{}
	ctx, cancel := context.WithCancel(context.Background())
	uc := app.NewPlaceOrder(cancellingRepo{newMemRepo(), cancel}, fixedClock{testNow}, stock)

	_, _ = uc.Execute(ctx, placeCmd(domain.Item{SKU: "a", Quantity: 2}))

	if len(stock.releaseCtxErr) != 1 || stock.releaseCtxErr[0] != nil {
		t.Fatalf("release saw ctx errors %v, want one call with a live context", stock.releaseCtxErr)
	}
}

func TestPlaceOrderStillAnswersWithTheExistingOrderWhenReleasingTheDuplicateFails(t *testing.T) {
	stock := &failingReleaseReserver{}
	uc := app.NewPlaceOrder(racedRepo{newMemRepo()}, fixedClock{testNow}, stock)

	order, err := uc.Execute(context.Background(), placeCmd(domain.Item{SKU: "a", Quantity: 2}))

	if err != nil || order == nil || order.ID != "winner" {
		t.Fatalf("order = %+v, err = %v: the order exists, so the placement succeeded", order, err)
	}
}

type failingReleaseReserver struct{ fakeReserver }

func (f *failingReleaseReserver) Release(context.Context, string, int) error { return errors.New("stock down") }
