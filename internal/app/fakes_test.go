package app_test

import (
	"context"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

type fakeCatalog struct{ prices map[string]int64 }

func (f fakeCatalog) UnitPriceCents(_ context.Context, sku string) (int64, error) {
	return f.prices[sku], nil
}

type fakeStock struct{ levels map[string]int }

func (f fakeStock) Available(_ context.Context, sku string) (int, error) {
	return f.levels[sku], nil
}

type fakeTMS struct {
	sla         domain.ShippingSLA
	tracking    string
	shipmentErr error
	shipments   int
}

func (f *fakeTMS) QuoteShipping(context.Context, []domain.Item) (domain.ShippingSLA, error) {
	return f.sla, nil
}

func (f *fakeTMS) CreateShipment(context.Context, domain.Order) (string, error) {
	f.shipments++
	if f.shipmentErr != nil {
		return "", f.shipmentErr
	}
	return f.tracking, nil
}

type fakeERP struct {
	err   error
	calls int
}

func (f *fakeERP) SendOrder(context.Context, domain.Order) error {
	f.calls++
	return f.err
}

type fakeMarketplace struct {
	err      error
	notified map[string]string
}

func (f *fakeMarketplace) NotifyInvoice(_ context.Context, orderID, tracking string) error {
	if f.err != nil {
		return f.err
	}
	if f.notified == nil {
		f.notified = map[string]string{}
	}
	f.notified[orderID] = tracking
	return nil
}

type memRepo struct {
	byID  map[string]domain.Order
	saves int
}

func newMemRepo() *memRepo { return &memRepo{byID: map[string]domain.Order{}} }

func (r *memRepo) Save(_ context.Context, o *domain.Order) error {
	r.saves++
	cp := *o
	cp.Items = append([]domain.Item(nil), o.Items...)
	r.byID[o.ID] = cp
	return nil
}

func (r *memRepo) Get(_ context.Context, id string) (*domain.Order, error) {
	o, ok := r.byID[id]
	if !ok {
		return nil, domain.ErrOrderNotFound
	}
	return &o, nil
}

func (r *memRepo) FindByIdempotencyKey(_ context.Context, key string) (*domain.Order, error) {
	for _, o := range r.byID {
		if o.IdempotencyKey == key {
			cp := o
			return &cp, nil
		}
	}
	return nil, domain.ErrOrderNotFound
}

type fakeBus struct {
	err    error
	events []domain.Event
}

func (b *fakeBus) Publish(_ context.Context, e domain.Event) error {
	if b.err != nil {
		return b.err
	}
	b.events = append(b.events, e)
	return nil
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }
