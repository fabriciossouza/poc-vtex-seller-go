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

func TestFulfillOrderRetryAfterPublishFailureStillPublishes(t *testing.T) {
	repo, bus := newMemRepo(), &fakeBus{err: errors.New("redis down")}
	seedOrder(t, repo)
	uc := app.NewFulfillOrder(repo, bus)
	if err := uc.Execute(context.Background(), "ord-1"); err == nil {
		t.Fatal("first call must report the publish failure")
	}
	bus.err = nil

	if err := uc.Execute(context.Background(), "ord-1"); err != nil {
		t.Fatalf("retry: %v", err)
	}

	want := domain.Event{Type: domain.EventOrderAuthorized, OrderID: "ord-1"}
	if len(bus.events) != 1 || bus.events[0] != want {
		t.Fatalf("events = %+v", bus.events)
	}
}

func TestFulfillOrderOfAnAlreadyAuthorizedOrderSucceedsAndRepublishes(t *testing.T) {
	repo, bus := newMemRepo(), &fakeBus{}
	seedOrder(t, repo)
	uc := app.NewFulfillOrder(repo, bus)
	_ = uc.Execute(context.Background(), "ord-1")

	if err := uc.Execute(context.Background(), "ord-1"); err != nil {
		t.Fatalf("second call: %v", err)
	}

	if len(bus.events) != 2 {
		t.Fatalf("events = %d, want 2 (the worker tolerates duplicates)", len(bus.events))
	}
}

func TestFulfillOrderOfAnOrderAlreadyBeyondAuthorizedSucceedsWithoutEvent(t *testing.T) {
	for name, advance := range map[string]func(*domain.Order){
		"sent to ERP": func(o *domain.Order) { _ = o.MarkSentToERP() },
		"shipped":     func(o *domain.Order) { _ = o.MarkSentToERP(); _ = o.MarkShipped("T") },
	} {
		t.Run(name, func(t *testing.T) {
			repo, bus := newMemRepo(), &fakeBus{}
			seedOrder(t, repo)
			o, _ := repo.Get(context.Background(), "ord-1")
			_ = o.Authorize()
			advance(o)
			_ = repo.Save(context.Background(), o)

			if err := app.NewFulfillOrder(repo, bus).Execute(context.Background(), "ord-1"); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(bus.events) != 0 {
				t.Fatalf("events = %+v", bus.events)
			}
		})
	}
}

func TestFulfillOrderRefusesACancelledOrder(t *testing.T) {
	repo, bus := newMemRepo(), &fakeBus{}
	seedOrder(t, repo)
	_ = app.NewCancelOrder(repo).Execute(context.Background(), "ord-1")

	err := app.NewFulfillOrder(repo, bus).Execute(context.Background(), "ord-1")

	if !errors.Is(err, domain.ErrInvalidTransition) || len(bus.events) != 0 {
		t.Fatalf("err = %v, events = %+v", err, bus.events)
	}
}

func TestFulfillOrderUnknownOrder(t *testing.T) {
	err := app.NewFulfillOrder(newMemRepo(), &fakeBus{}).Execute(context.Background(), "nope")
	if !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("err = %v", err)
	}
}
