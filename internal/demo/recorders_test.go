package demo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/demo"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

var testOrder = domain.Order{ID: "ord-1", IdempotencyKey: "mkt-1", TrackingCode: "TRK-9"}

type erpStub struct{ err error }

func (s erpStub) SendOrder(context.Context, domain.Order) error { return s.err }

type tmsStub struct{ err error }

func (s tmsStub) QuoteShipping(context.Context, []domain.Item) (domain.ShippingSLA, error) {
	return domain.ShippingSLA{PriceCents: 1}, s.err
}

func (s tmsStub) CreateShipment(context.Context, domain.Order) (string, error) {
	return "TRK-9", s.err
}

type marketplaceStub struct{ err error }

func (s marketplaceStub) NotifyInvoice(context.Context, domain.Order) error { return s.err }

type busStub struct{}

func (busStub) Publish(context.Context, domain.Event) error { return nil }

func last(t *testing.T, tl *demo.Timeline) demo.Event {
	t.Helper()
	events := tl.Events("mkt-1")
	if len(events) == 0 {
		t.Fatal("nothing was recorded")
	}
	return events[len(events)-1]
}

func TestRecordingERPNotesSuccessAndFailureAndPassesTheErrorThrough(t *testing.T) {
	tl := newTimeline()
	boom := errors.New("erp responded with status 503")

	if err := demo.NewRecordingERP(erpStub{}, tl).SendOrder(context.Background(), testOrder); err != nil {
		t.Fatal(err)
	}
	if e := last(t, tl); e.Source != "erp" || e.Level != demo.OK {
		t.Fatalf("event = %+v", e)
	}
	err := demo.NewRecordingERP(erpStub{boom}, tl).SendOrder(context.Background(), testOrder)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the original error", err)
	}
	if e := last(t, tl); e.Level != demo.Error || !strings.Contains(e.Message, "503") {
		t.Fatalf("event = %+v", e)
	}
}

func TestRecordingTMSNotesTheTrackingCodeAndOnlyTheShipment(t *testing.T) {
	tl := newTimeline()
	rec := demo.NewRecordingTMS(tmsStub{}, tl)

	if _, err := rec.QuoteShipping(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(tl.Events("mkt-1")) != 0 {
		t.Fatal("quotes belong to no order and must not be recorded")
	}
	code, err := rec.CreateShipment(context.Background(), testOrder)
	if err != nil || code != "TRK-9" {
		t.Fatalf("code = %q, err = %v", code, err)
	}
	if e := last(t, tl); e.Source != "tms" || !strings.Contains(e.Message, "TRK-9") {
		t.Fatalf("event = %+v", e)
	}
}

func TestRecordingMarketplaceNotesTheInvoiceNotification(t *testing.T) {
	tl := newTimeline()

	_ = demo.NewRecordingMarketplace(marketplaceStub{errors.New("marketplace responded with status 500")}, tl).NotifyInvoice(context.Background(), testOrder)

	if e := last(t, tl); e.Source != "marketplace" || e.Level != demo.Error {
		t.Fatalf("event = %+v", e)
	}
}

func TestRecordingBusNotesThePublishedEventUnderTheSellerOrder(t *testing.T) {
	tl := newTimeline()
	tl.Alias("ord-1", "mkt-1")

	err := demo.NewRecordingBus(busStub{}, tl).Publish(context.Background(), domain.Event{Type: domain.EventOrderAuthorized, OrderID: "ord-1"})

	if err != nil {
		t.Fatal(err)
	}
	if e := last(t, tl); e.Source != "fila" || !strings.Contains(e.Message, "order.authorized") {
		t.Fatalf("event = %+v", e)
	}
}

type consumerStub struct {
	delivery ports.Delivery
	rejected int
}

func (c *consumerStub) Receive(context.Context) (ports.Delivery, error) { return c.delivery, nil }
func (c *consumerStub) Ack(context.Context, string) error               { return nil }
func (c *consumerStub) Reject(context.Context, ports.Delivery, string, int) error {
	c.rejected++
	return nil
}
func (c *consumerStub) DeadLetters(context.Context) ([]ports.DeadLetter, error) { return nil, nil }
func (c *consumerStub) Discard(context.Context, string) error                   { return nil }

func TestRecordingConsumerNotesAckAndDeadLetteringWithReasonAndAttempts(t *testing.T) {
	ctx := context.Background()
	tl := newTimeline()
	tl.Alias("ord-1", "mkt-1")
	inner := &consumerStub{delivery: ports.Delivery{ID: "1-0", Event: domain.Event{Type: domain.EventOrderAuthorized, OrderID: "ord-1"}}}
	rec := demo.NewRecordingConsumer(inner, tl)

	d, err := rec.Receive(ctx)
	if err != nil || d.ID != "1-0" {
		t.Fatalf("Receive = %+v, %v", d, err)
	}
	_ = rec.Ack(ctx, "1-0")
	if e := last(t, tl); e.Source != "fila" || e.Level != demo.OK {
		t.Fatalf("ack event = %+v", e)
	}
	_ = rec.Reject(ctx, d, "circuit breaker is open", 4)

	e := last(t, tl)
	if e.Source != "dlq" || e.Level != demo.Error || !strings.Contains(e.Message, "circuit breaker is open") || !strings.Contains(e.Message, "4") {
		t.Fatalf("reject event = %+v", e)
	}
	if inner.rejected != 1 {
		t.Fatal("Reject must reach the real consumer")
	}
}
