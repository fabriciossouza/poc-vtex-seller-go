package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type CancelOrder struct {
	repo  ports.OrderRepo
	stock ports.StockReserver
}

func NewCancelOrder(repo ports.OrderRepo, stock ports.StockReserver) *CancelOrder {
	return &CancelOrder{repo: repo, stock: stock}
}

func (uc *CancelOrder) Execute(ctx context.Context, orderID, marketplaceOrderID string) error {
	order, err := uc.repo.Get(ctx, orderID)
	if err != nil {
		return fmt.Errorf("get order %s: %w", orderID, err)
	}
	if order.IdempotencyKey != marketplaceOrderID {
		return fmt.Errorf("order %s: %w", orderID, domain.ErrOrderNotFound)
	}
	if order.Status == domain.StatusSentToERP || order.Status == domain.StatusShipped {
		return fmt.Errorf("order %s is %s: %w", orderID, order.Status, domain.ErrCancellationEvaluating)
	}
	if err := order.Cancel(); err != nil {
		return err
	}
	if err := uc.repo.Save(ctx, order); err != nil {
		return fmt.Errorf("save order %s: %w", orderID, err)
	}
	// Only a saved cancellation gives the reservation back, and a second
	// cancel is rejected by the transition above, so it never releases twice.
	if err := releaseStock(ctx, uc.stock, order.Items); err != nil {
		slog.Error("cancel: stock release failed", "order", orderID, "error", err)
	}
	return nil
}
