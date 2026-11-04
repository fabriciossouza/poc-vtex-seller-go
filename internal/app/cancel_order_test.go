package app_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

func TestCancelOrderCancelsAReceivedOrder(t *testing.T) {
	repo := newMemRepo()
	seedOrder(t, repo)

	if err := app.NewCancelOrder(repo, &fakeReserver{}).Execute(context.Background(), "ord-1"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := repo.Get(context.Background(), "ord-1")
	if got.Status != domain.StatusCancelled {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestCancelOrderRefusesShippedOrder(t *testing.T) {
	repo := newMemRepo()
	seedOrder(t, repo)
	o, _ := repo.Get(context.Background(), "ord-1")
	_ = o.Authorize()
	_ = o.MarkSentToERP()
	_ = o.MarkShipped("TRK-1")
	_ = repo.Save(context.Background(), o)

	err := app.NewCancelOrder(repo, &fakeReserver{}).Execute(context.Background(), "ord-1")

	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	got, _ := repo.Get(context.Background(), "ord-1")
	if got.Status != domain.StatusShipped {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestCancelOrderUnknownOrder(t *testing.T) {
	err := app.NewCancelOrder(newMemRepo(), &fakeReserver{}).Execute(context.Background(), "nope")
	if !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestCancelOrderReleasesTheReservedStockOnlyWhenTheCancellationIsSaved(t *testing.T) {
	for _, status := range []domain.OrderStatus{domain.StatusReceived, domain.StatusAuthorized, domain.StatusSentToERP} {
		repo, stock := newMemRepo(), &fakeReserver{}
		seedOrder(t, repo)
		o, _ := repo.Get(context.Background(), "ord-1")
		if status != domain.StatusReceived {
			_ = o.Authorize()
		}
		if status == domain.StatusSentToERP {
			_ = o.MarkSentToERP()
		}
		_ = repo.Save(context.Background(), o)

		if err := app.NewCancelOrder(repo, stock).Execute(context.Background(), "ord-1"); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if want := []string{"release a 1"}; !reflect.DeepEqual(stock.calls, want) {
			t.Fatalf("%s: calls = %v, want %v", status, stock.calls, want)
		}
		// A repeated cancellation must not release a second time.
		_ = app.NewCancelOrder(repo, stock).Execute(context.Background(), "ord-1")
		if len(stock.calls) != 1 {
			t.Fatalf("%s: repeated cancel released again: %v", status, stock.calls)
		}
	}

	repo, stock := newMemRepo(), &fakeReserver{}
	seedOrder(t, repo)
	o, _ := repo.Get(context.Background(), "ord-1")
	_ = o.Authorize()
	_ = o.MarkSentToERP()
	_ = o.MarkShipped("TRK")
	_ = repo.Save(context.Background(), o)
	_ = app.NewCancelOrder(repo, stock).Execute(context.Background(), "ord-1")
	if len(stock.calls) != 0 {
		t.Fatalf("a shipped order keeps its stock: %v", stock.calls)
	}
}

func TestCancelOrderDoesNotReleaseWhenTheSaveLosesTheRace(t *testing.T) {
	repo, stock := newMemRepo(), &fakeReserver{}
	seedOrder(t, repo)
	racing := conflictOnce{memRepo: repo}

	err := app.NewCancelOrder(&racing, stock).Execute(context.Background(), "ord-1")

	if !errors.Is(err, ports.ErrVersionConflict) || len(stock.calls) != 0 {
		t.Fatalf("err = %v, calls = %v", err, stock.calls)
	}
}

type conflictOnce struct{ *memRepo }

func (conflictOnce) Save(context.Context, *domain.Order) error { return ports.ErrVersionConflict }
