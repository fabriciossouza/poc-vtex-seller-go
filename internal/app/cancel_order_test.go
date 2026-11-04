package app_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

func TestCancelOrderCancelsAReceivedOrder(t *testing.T) {
	repo := newMemRepo()
	seedOrder(t, repo)

	if err := app.NewCancelOrder(repo, &fakeReserver{}).Execute(context.Background(), "ord-1", "key-1"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := repo.Get(context.Background(), "ord-1")
	if got.Status != domain.StatusCancelled {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestCancelOrderOnlyEvaluatesWhenTheOrderIsAlreadyAtTheERPOrShipped(t *testing.T) {
	for _, status := range []domain.OrderStatus{domain.StatusSentToERP, domain.StatusShipped} {
		repo, stock := newMemRepo(), &fakeReserver{}
		seedOrder(t, repo)
		o, _ := repo.Get(context.Background(), "ord-1")
		_ = o.Authorize()
		_ = o.MarkSentToERP()
		if status == domain.StatusShipped {
			_ = o.MarkShipped("TRK-1")
		}
		_ = repo.Save(context.Background(), o)

		err := app.NewCancelOrder(repo, stock).Execute(context.Background(), "ord-1", "key-1")

		if !errors.Is(err, domain.ErrCancellationEvaluating) {
			t.Fatalf("%s: err = %v, want ErrCancellationEvaluating", status, err)
		}
		got, _ := repo.Get(context.Background(), "ord-1")
		if got.Status != status || len(stock.calls) != 0 {
			t.Fatalf("%s: status = %s, stock calls = %v; goods may already be moving, so nothing changes", status, got.Status, stock.calls)
		}
	}
}

func TestCancelOrderUnknownOrder(t *testing.T) {
	err := app.NewCancelOrder(newMemRepo(), &fakeReserver{}).Execute(context.Background(), "nope", "key-1")
	if !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestCancelOrderReleasesTheReservedStockOnlyWhenTheCancellationIsSaved(t *testing.T) {
	for _, status := range []domain.OrderStatus{domain.StatusReceived, domain.StatusAuthorized} {
		repo, stock := newMemRepo(), &fakeReserver{}
		seedOrder(t, repo)
		o, _ := repo.Get(context.Background(), "ord-1")
		if status != domain.StatusReceived {
			_ = o.Authorize()
		}
		_ = repo.Save(context.Background(), o)

		if err := app.NewCancelOrder(repo, stock).Execute(context.Background(), "ord-1", "key-1"); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if want := []string{"release a 1"}; !reflect.DeepEqual(stock.calls, want) {
			t.Fatalf("%s: calls = %v, want %v", status, stock.calls, want)
		}
		// A repeated cancellation must not release a second time.
		_ = app.NewCancelOrder(repo, stock).Execute(context.Background(), "ord-1", "key-1")
		if len(stock.calls) != 1 {
			t.Fatalf("%s: repeated cancel released again: %v", status, stock.calls)
		}
	}
}

func TestCancelOrderDoesNotReleaseWhenTheSaveLosesTheRace(t *testing.T) {
	repo, stock := newMemRepo(), &fakeReserver{}
	seedOrder(t, repo)
	racing := conflictOnce{memRepo: repo}

	err := app.NewCancelOrder(&racing, stock).Execute(context.Background(), "ord-1", "key-1")

	if !errors.Is(err, ports.ErrVersionConflict) || len(stock.calls) != 0 {
		t.Fatalf("err = %v, calls = %v", err, stock.calls)
	}
}

type conflictOnce struct{ *memRepo }

func (conflictOnce) Save(context.Context, *domain.Order) error { return ports.ErrVersionConflict }

func TestCancelOrderRepeatedOnACancelledOrderSucceedsWithoutReleasingTwice(t *testing.T) {
	repo, stock := newMemRepo(), &fakeReserver{}
	seedOrder(t, repo)
	uc := app.NewCancelOrder(repo, stock)
	_ = uc.Execute(context.Background(), "ord-1", "key-1")

	if err := uc.Execute(context.Background(), "ord-1", "key-1"); err != nil {
		t.Fatalf("repeated cancel: %v", err)
	}
	if len(stock.calls) != 1 {
		t.Fatalf("stock calls = %v, want a single release", stock.calls)
	}
	if err := uc.Execute(context.Background(), "ord-1", "other"); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("wrong marketplaceOrderId on a cancelled order: err = %v, want ErrOrderNotFound", err)
	}
}

func TestCancelOrderLogsWhenItAnswersEvaluating(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	repo := newMemRepo()
	seedOrder(t, repo)
	o, _ := repo.Get(context.Background(), "ord-1")
	_ = o.Authorize()
	_ = o.MarkSentToERP()
	_ = repo.Save(context.Background(), o)

	_ = app.NewCancelOrder(repo, &fakeReserver{}).Execute(context.Background(), "ord-1", "key-1")

	if out := logs.String(); !strings.Contains(out, "order=ord-1") || !strings.Contains(out, "status=sent_to_erp") {
		t.Fatalf("log = %q, want the order id and status of the cancel answered as evaluating", out)
	}
}
