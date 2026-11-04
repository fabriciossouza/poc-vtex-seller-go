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

func (uc *FulfillOrder) Execute(ctx context.Context, orderID string) error {
	order, err := uc.repo.Get(ctx, orderID)
	if err != nil {
		return fmt.Errorf("get order %s: %w", orderID, err)
	}
	if err := order.Authorize(); err != nil {
		return err
	}
	if err := uc.repo.Save(ctx, order); err != nil {
		return fmt.Errorf("save order %s: %w", orderID, err)
	}
	if err := uc.bus.Publish(ctx, domain.Event{Type: domain.EventOrderAuthorized, OrderID: orderID}); err != nil {
		return fmt.Errorf("publish %s: %w", domain.EventOrderAuthorized, err)
	}
	return nil
}
