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

	MarketplaceEndpoint string
}

type PlaceOrder struct {
	repo  ports.OrderRepo
	clock ports.Clock
	stock ports.StockReserver
}

func NewPlaceOrder(repo ports.OrderRepo, clock ports.Clock, stock ports.StockReserver) *PlaceOrder {
	return &PlaceOrder{repo: repo, clock: clock, stock: stock}
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
	order.MarketplaceEndpoint = cmd.MarketplaceEndpoint
	if err := uc.reserve(ctx, order.Items); err != nil {
		return nil, err
	}
	if err := uc.repo.Save(ctx, order); err != nil {
		return nil, errors.Join(fmt.Errorf("save order: %w", err), uc.release(ctx, order.Items))
	}
	if order.ID != cmd.OrderID {
		// A concurrent placement of the same key won the insert and holds the reservation.
		if err := uc.release(ctx, cmd.Items); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func (uc *PlaceOrder) reserve(ctx context.Context, items []domain.Item) error {
	for i, it := range items {
		if err := uc.stock.Reserve(ctx, it.SKU, it.Quantity); err != nil {
			return errors.Join(fmt.Errorf("reserve %s: %w", it.SKU, err), uc.release(ctx, items[:i]))
		}
	}
	return nil
}

func (uc *PlaceOrder) release(ctx context.Context, items []domain.Item) error {
	var errs []error
	for _, it := range items {
		if err := uc.stock.Release(ctx, it.SKU, it.Quantity); err != nil {
			errs = append(errs, fmt.Errorf("release %s: %w", it.SKU, err))
		}
	}
	return errors.Join(errs...)
}
