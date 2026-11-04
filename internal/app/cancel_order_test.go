package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

func TestCancelOrderCancelsAReceivedOrder(t *testing.T) {
	repo := newMemRepo()
	seedOrder(t, repo)

	if err := app.NewCancelOrder(repo).Execute(context.Background(), "ord-1"); err != nil {
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

	err := app.NewCancelOrder(repo).Execute(context.Background(), "ord-1")

	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	got, _ := repo.Get(context.Background(), "ord-1")
	if got.Status != domain.StatusShipped {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestCancelOrderUnknownOrder(t *testing.T) {
	err := app.NewCancelOrder(newMemRepo()).Execute(context.Background(), "nope")
	if !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("err = %v", err)
	}
}
