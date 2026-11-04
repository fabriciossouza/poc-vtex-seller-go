package ports

import (
	"context"
	"errors"
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
// Updating an order whose Version no longer matches the stored one returns
// ErrVersionConflict, and a successful Save bumps order.Version.
// When Save would insert an order whose idempotency key already belongs to
// another order (a concurrent placement), it overwrites *order with the stored
// one and returns nil, so the caller proceeds with the single surviving order.
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

var ErrNoDelivery = errors.New("no delivery available")

type Delivery struct {
	ID    string
	Event domain.Event
}

type DeadLetter struct {
	ID       string
	Event    domain.Event
	Reason   string
	Attempts int
	FailedAt time.Time
}

// EventConsumer is the receiving side of the queue. A delivery stays pending
// (and is redelivered after a while) until it is acked or rejected.
type EventConsumer interface {
	// Receive waits a bounded time and returns ErrNoDelivery when nothing arrived.
	Receive(ctx context.Context) (Delivery, error)
	Ack(ctx context.Context, deliveryID string) error
	// Reject moves the delivery to the dead-letter queue and settles it.
	Reject(ctx context.Context, delivery Delivery, reason string, attempts int) error
	DeadLetters(ctx context.Context) ([]DeadLetter, error)
	// Discard removes a dead letter once it was reprocessed.
	Discard(ctx context.Context, deadLetterID string) error
}
