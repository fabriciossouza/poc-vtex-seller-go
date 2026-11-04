package app

import (
	"context"
	"fmt"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type DispatchToERP struct {
	repo        ports.OrderRepo
	erp         ports.ERPPort
	tms         ports.TMSPort
	marketplace ports.MarketplacePort
}

func NewDispatchToERP(repo ports.OrderRepo, erp ports.ERPPort, tms ports.TMSPort, marketplace ports.MarketplacePort) *DispatchToERP {
	return &DispatchToERP{repo: repo, erp: erp, tms: tms, marketplace: marketplace}
}

// Execute resumes from the order's current status, so a caller that retries
// after a port failure never repeats a step that already succeeded.
func (uc *DispatchToERP) Execute(ctx context.Context, orderID string) error {
	order, err := uc.repo.Get(ctx, orderID)
	if err != nil {
		return fmt.Errorf("get order %s: %w", orderID, err)
	}

	switch order.Status {
	case domain.StatusAuthorized:
		if err := uc.sendToERP(ctx, order); err != nil {
			return err
		}
		fallthrough
	case domain.StatusSentToERP:
		if err := uc.ship(ctx, order); err != nil {
			return err
		}
		fallthrough
	case domain.StatusShipped:
		if err := uc.marketplace.NotifyInvoice(ctx, order.ID, order.TrackingCode); err != nil {
			return fmt.Errorf("notify marketplace %s: %w", order.ID, err)
		}
		return nil
	default:
		return &domain.TransitionError{From: order.Status, To: domain.StatusSentToERP}
	}
}

func (uc *DispatchToERP) sendToERP(ctx context.Context, order *domain.Order) error {
	if err := uc.erp.SendOrder(ctx, *order); err != nil {
		return fmt.Errorf("send order %s to ERP: %w", order.ID, err)
	}
	return uc.advance(ctx, order, order.MarkSentToERP)
}

func (uc *DispatchToERP) ship(ctx context.Context, order *domain.Order) error {
	tracking, err := uc.tms.CreateShipment(ctx, *order)
	if err != nil {
		return fmt.Errorf("create shipment for %s: %w", order.ID, err)
	}
	return uc.advance(ctx, order, func() error { return order.MarkShipped(tracking) })
}

func (uc *DispatchToERP) advance(ctx context.Context, order *domain.Order, transition func() error) error {
	if err := transition(); err != nil {
		return err
	}
	if err := uc.repo.Save(ctx, order); err != nil {
		return fmt.Errorf("save order %s: %w", order.ID, err)
	}
	return nil
}
