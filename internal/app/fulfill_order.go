package app

import (
	"context"
	"fmt"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type FulfillOrder struct {
	repo ports.OrderRepo
	bus  ports.EventBus
}

func NewFulfillOrder(repo ports.OrderRepo, bus ports.EventBus) *FulfillOrder {
	return &FulfillOrder{repo: repo, bus: bus}
}

// Execute is idempotent: the marketplace retries until it gets a 2xx, so an
// already authorized order publishes its event again instead of failing.
func (uc *FulfillOrder) Execute(ctx context.Context, orderID, marketplaceOrderID string) error {
	order, err := uc.repo.Get(ctx, orderID)
	if err != nil {
		return fmt.Errorf("get order %s: %w", orderID, err)
	}
	if order.IdempotencyKey != marketplaceOrderID {
		return fmt.Errorf("order %s: %w", orderID, domain.ErrOrderNotFound)
	}
	switch order.Status {
	case domain.StatusSentToERP, domain.StatusShipped:
		return nil
	case domain.StatusAuthorized:
		// A previous call may have saved the order and lost the event.
	default:
		if err := order.Authorize(); err != nil {
			return err
		}
		if err := uc.repo.Save(ctx, order); err != nil {
			return fmt.Errorf("save order %s: %w", orderID, err)
		}
	}
	if err := uc.bus.Publish(ctx, domain.Event{Type: domain.EventOrderAuthorized, OrderID: orderID}); err != nil {
		return fmt.Errorf("publish %s: %w", domain.EventOrderAuthorized, err)
	}
	return nil
}
