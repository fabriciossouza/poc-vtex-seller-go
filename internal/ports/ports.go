package ports

import (
	"context"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

type Catalog interface {
	UnitPriceCents(ctx context.Context, sku string) (int64, error)
}

type Stock interface {
	Available(ctx context.Context, sku string) (int, error)
}

type ERPPort interface {
	SendOrder(ctx context.Context, order domain.Order) error
}

type TMSPort interface {
	QuoteShipping(ctx context.Context, items []domain.Item) (domain.ShippingSLA, error)
	CreateShipment(ctx context.Context, order domain.Order) (trackingCode string, err error)
}

// MarketplacePort notifies the invoice of a shipped order. The marketplace
// order id is order.IdempotencyKey. Repeated notifications must be tolerated.
type MarketplacePort interface {
	NotifyInvoice(ctx context.Context, order domain.Order) error
}

// OrderRepo implementations return domain.ErrOrderNotFound from Get and
// FindByIdempotencyKey when nothing matches. Returned orders are copies.
type OrderRepo interface {
	Save(ctx context.Context, order *domain.Order) error
	Get(ctx context.Context, id string) (*domain.Order, error)
	FindByIdempotencyKey(ctx context.Context, key string) (*domain.Order, error)
}

type EventBus interface {
	Publish(ctx context.Context, event domain.Event) error
}

type Clock interface {
	Now() time.Time
}
