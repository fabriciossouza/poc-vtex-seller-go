package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type PlaceOrderCommand struct {
	OrderID        string
	IdempotencyKey string
	Items          []domain.Item
}

type PlaceOrder struct {
	repo  ports.OrderRepo
	clock ports.Clock
}

func NewPlaceOrder(repo ports.OrderRepo, clock ports.Clock) *PlaceOrder {
	return &PlaceOrder{repo: repo, clock: clock}
}

func (uc *PlaceOrder) Execute(ctx context.Context, cmd PlaceOrderCommand) (*domain.Order, error) {
	existing, err := uc.repo.FindByIdempotencyKey(ctx, cmd.IdempotencyKey)
	switch {
	case err == nil:
		return existing, nil
	case !errors.Is(err, domain.ErrOrderNotFound):
		return nil, fmt.Errorf("find by idempotency key: %w", err)
	}

	order, err := domain.NewOrder(cmd.OrderID, cmd.IdempotencyKey, cmd.Items, uc.clock.Now())
	if err != nil {
		return nil, err
	}
	if err := uc.repo.Save(ctx, order); err != nil {
		return nil, fmt.Errorf("save order: %w", err)
	}
	return order, nil
}
