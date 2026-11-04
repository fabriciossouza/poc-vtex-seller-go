package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

func seedOrder(t *testing.T, repo *memRepo) {
	t.Helper()
	o, err := domain.NewOrder("ord-1", "key-1", []domain.Item{{SKU: "a", Quantity: 1}}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

func TestFulfillOrderAuthorizesAndPublishesEvent(t *testing.T) {
	repo, bus := newMemRepo(), &fakeBus{}
	seedOrder(t, repo)
	uc := app.NewFulfillOrder(repo, bus)

	if err := uc.Execute(context.Background(), "ord-1"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := repo.Get(context.Background(), "ord-1")
	if got.Status != domain.StatusAuthorized {
		t.Fatalf("status = %s", got.Status)
	}
	want := domain.Event{Type: domain.EventOrderAuthorized, OrderID: "ord-1"}
	if len(bus.events) != 1 || bus.events[0] != want {
		t.Fatalf("events = %+v", bus.events)
	}
}

func TestFulfillOrderRejectsSecondAuthorizationWithoutNewEvent(t *testing.T) {
	repo, bus := newMemRepo(), &fakeBus{}
	seedOrder(t, repo)
	uc := app.NewFulfillOrder(repo, bus)
	_ = uc.Execute(context.Background(), "ord-1")

	err := uc.Execute(context.Background(), "ord-1")

	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if len(bus.events) != 1 {
		t.Fatalf("events = %d, want 1", len(bus.events))
	}
}

func TestFulfillOrderUnknownOrder(t *testing.T) {
	err := app.NewFulfillOrder(newMemRepo(), &fakeBus{}).Execute(context.Background(), "nope")
	if !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("err = %v", err)
	}
}
