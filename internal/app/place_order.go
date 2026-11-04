package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type PlaceOrderCommand struct {
	OrderID        string
	IdempotencyKey string
	Items          []domain.Item

	MarketplaceEndpoint string
	FreightCents        int64
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
	if err := order.SetFreight(cmd.FreightCents); err != nil {
		return nil, err
	}
	built, items := order.ID, append([]domain.Item(nil), order.Items...)
	if err := uc.reserve(ctx, items); err != nil {
		return nil, err
	}
	if err := uc.repo.Save(ctx, order); err != nil {
		return nil, errors.Join(fmt.Errorf("save order: %w", err), releaseStock(ctx, uc.stock, items))
	}
	if order.ID != built {
		// A concurrent placement of the same key won the insert and holds the
		// reservation. The order exists, so a failed release is only logged.
		if err := releaseStock(ctx, uc.stock, items); err != nil {
			slog.Error("place: releasing the duplicate reservation failed", "order", order.ID, "error", err)
		}
	}
	return order, nil
}

func (uc *PlaceOrder) reserve(ctx context.Context, items []domain.Item) error {
	for i, it := range items {
		if err := uc.stock.Reserve(ctx, it.SKU, it.Quantity); err != nil {
			return errors.Join(fmt.Errorf("reserve %s: %w", it.SKU, err), releaseStock(ctx, uc.stock, items[:i]))
		}
	}
	return nil
}
