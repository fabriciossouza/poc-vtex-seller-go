package app

import (
	"context"
	"fmt"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type CancelOrder struct {
	repo ports.OrderRepo
}

func NewCancelOrder(repo ports.OrderRepo) *CancelOrder {
	return &CancelOrder{repo: repo}
}

func (uc *CancelOrder) Execute(ctx context.Context, orderID string) error {
	order, err := uc.repo.Get(ctx, orderID)
	if err != nil {
		return fmt.Errorf("get order %s: %w", orderID, err)
	}
	if err := order.Cancel(); err != nil {
		return err
	}
	if err := uc.repo.Save(ctx, order); err != nil {
		return fmt.Errorf("save order %s: %w", orderID, err)
	}
	return nil
}
