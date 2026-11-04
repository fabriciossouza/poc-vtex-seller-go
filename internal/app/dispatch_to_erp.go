package app

import (
	"context"
	"errors"
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

// errCancelled stops the flow when a cancellation was confirmed meanwhile.
var errCancelled = errors.New("order was cancelled")

// Execute resumes from the order's current status, so a caller that retries
// after a port failure never repeats a step that already succeeded. A
// cancelled order is left alone: the cancellation was already confirmed to
// the marketplace.
func (uc *DispatchToERP) Execute(ctx context.Context, orderID string) error {
	if err := uc.run(ctx, orderID); err != nil && !errors.Is(err, errCancelled) {
		return err
	}
	return nil
}

func (uc *DispatchToERP) run(ctx context.Context, orderID string) error {
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
		if err := uc.marketplace.NotifyInvoice(ctx, *order); err != nil {
			return fmt.Errorf("notify marketplace %s: %w", order.ID, err)
		}
		return nil
	case domain.StatusCancelled:
		return fmt.Errorf("order %s: %w", order.ID, ports.ErrNothingDispatched)
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
	err := uc.repo.Save(ctx, order)
	if errors.Is(err, ports.ErrVersionConflict) {
		return uc.afterConflict(ctx, order.ID, err)
	}
	if err != nil {
		return fmt.Errorf("save order %s: %w", order.ID, err)
	}
	return nil
}

// afterConflict stops if the conflicting write was a cancellation; for any
// other writer it reports the conflict so the caller retries from the new state.
func (uc *DispatchToERP) afterConflict(ctx context.Context, orderID string, conflict error) error {
	current, err := uc.repo.Get(ctx, orderID)
	if err == nil && current.Status == domain.StatusCancelled {
		return errCancelled
	}
	return fmt.Errorf("save order %s: %w", orderID, conflict)
}
