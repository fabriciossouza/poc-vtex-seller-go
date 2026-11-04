package domain

import (
	"errors"
	"fmt"
	"time"
)

type OrderStatus string

const (
	StatusReceived   OrderStatus = "received"
	StatusAuthorized OrderStatus = "authorized"
	StatusSentToERP  OrderStatus = "sent_to_erp"
	StatusShipped    OrderStatus = "shipped"
	StatusFailed     OrderStatus = "failed"
	StatusCancelled  OrderStatus = "cancelled"
)

var (
	ErrInvalidOrder      = errors.New("invalid order")
	ErrInvalidTransition = errors.New("invalid order transition")
	ErrOrderNotFound     = errors.New("order not found")
)

// TransitionError carries the rejected transition so callers can log or map it.
type TransitionError struct {
	From, To OrderStatus
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("invalid order transition: %s -> %s", e.From, e.To)
}

func (e *TransitionError) Is(target error) bool { return target == ErrInvalidTransition }

type Item struct {
	SKU        string
	Quantity   int
	PriceCents int64
}

type Order struct {
	ID             string
	IdempotencyKey string
	Items          []Item
	Status         OrderStatus
	TrackingCode   string
	CreatedAt      time.Time
	// MarketplaceEndpoint is where the marketplace expects this order's
	// invoice; it only arrives with the placement request.
	MarketplaceEndpoint string
}

func NewOrder(id, idempotencyKey string, items []Item, now time.Time) (*Order, error) {
	if id == "" || idempotencyKey == "" {
		return nil, fmt.Errorf("%w: id and idempotency key are required", ErrInvalidOrder)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: no items", ErrInvalidOrder)
	}
	for _, it := range items {
		if it.SKU == "" || it.Quantity <= 0 {
			return nil, fmt.Errorf("%w: item %q quantity %d", ErrInvalidOrder, it.SKU, it.Quantity)
		}
	}
	return &Order{
		ID:             id,
		IdempotencyKey: idempotencyKey,
		Items:          append([]Item(nil), items...),
		Status:         StatusReceived,
		CreatedAt:      now,
	}, nil
}

func (o *Order) Authorize() error { return o.transition(StatusAuthorized, StatusReceived) }

func (o *Order) MarkSentToERP() error { return o.transition(StatusSentToERP, StatusAuthorized) }

func (o *Order) MarkShipped(trackingCode string) error {
	if err := o.transition(StatusShipped, StatusSentToERP); err != nil {
		return err
	}
	o.TrackingCode = trackingCode
	return nil
}

func (o *Order) Fail() error {
	return o.transition(StatusFailed, StatusReceived, StatusAuthorized, StatusSentToERP)
}

func (o *Order) Cancel() error {
	return o.transition(StatusCancelled, StatusReceived, StatusAuthorized, StatusSentToERP)
}

func (o *Order) transition(to OrderStatus, allowedFrom ...OrderStatus) error {
	for _, from := range allowedFrom {
		if o.Status == from {
			o.Status = to
			return nil
		}
	}
	return &TransitionError{From: o.Status, To: to}
}
