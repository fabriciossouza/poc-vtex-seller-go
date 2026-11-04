package demo

import (
	"context"
	"fmt"
	"sync"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

// The recorders decorate the real ports so the demo can show each attempt
// without the core knowing about the timeline.

type RecordingERP struct {
	next ports.ERPPort
	tl   *Timeline
}

func NewRecordingERP(next ports.ERPPort, tl *Timeline) *RecordingERP {
	return &RecordingERP{next: next, tl: tl}
}

func (r *RecordingERP) SendOrder(ctx context.Context, order domain.Order) error {
	err := r.next.SendOrder(ctx, order)
	if err != nil {
		r.tl.Add(order.IdempotencyKey, "erp", Error, "ERP recusou ou não respondeu: "+err.Error())
		return err
	}
	r.tl.Add(order.IdempotencyKey, "erp", OK, "ERP registrou o pedido")
	return nil
}

type RecordingTMS struct {
	next ports.TMSPort
	tl   *Timeline
}

func NewRecordingTMS(next ports.TMSPort, tl *Timeline) *RecordingTMS {
	return &RecordingTMS{next: next, tl: tl}
}

// QuoteShipping is not recorded: a quote belongs to a cart, not to an order.
func (r *RecordingTMS) QuoteShipping(ctx context.Context, items []domain.Item) (domain.ShippingSLA, error) {
	return r.next.QuoteShipping(ctx, items)
}

func (r *RecordingTMS) CreateShipment(ctx context.Context, order domain.Order) (string, error) {
	code, err := r.next.CreateShipment(ctx, order)
	if err != nil {
		r.tl.Add(order.IdempotencyKey, "tms", Error, "TMS falhou ao criar a remessa: "+err.Error())
		return "", err
	}
	r.tl.Add(order.IdempotencyKey, "tms", OK, "TMS criou a remessa, rastreio "+code)
	return code, nil
}

type RecordingMarketplace struct {
	next ports.MarketplacePort
	tl   *Timeline
}

func NewRecordingMarketplace(next ports.MarketplacePort, tl *Timeline) *RecordingMarketplace {
	return &RecordingMarketplace{next: next, tl: tl}
}

func (r *RecordingMarketplace) NotifyInvoice(ctx context.Context, order domain.Order) error {
	// Recorded before the call: the simulated marketplace logs its receipt
	// while handling it.
	r.tl.Add(order.IdempotencyKey, "marketplace", Info, "seller envia a nota com o rastreio")
	if err := r.next.NotifyInvoice(ctx, order); err != nil {
		r.tl.Add(order.IdempotencyKey, "marketplace", Error, "seller não conseguiu enviar a nota: "+err.Error())
		return err
	}
	return nil
}

type RecordingBus struct {
	next ports.EventBus
	tl   *Timeline
}

func NewRecordingBus(next ports.EventBus, tl *Timeline) *RecordingBus {
	return &RecordingBus{next: next, tl: tl}
}

func (r *RecordingBus) Publish(ctx context.Context, event domain.Event) error {
	// Recorded before the call: a consumer may read the event before Publish
	// returns.
	r.tl.Add(event.OrderID, "fila", Info, fmt.Sprintf("seller publica o evento %s na fila", event.Type))
	if err := r.next.Publish(ctx, event); err != nil {
		r.tl.Add(event.OrderID, "fila", Error, fmt.Sprintf("falha ao publicar o evento %s: %s", event.Type, err.Error()))
		return err
	}
	return nil
}

// RecordingConsumer notes settled deliveries. Ack carries only the delivery
// id, so it remembers which order each delivery belongs to.
type RecordingConsumer struct {
	next ports.EventConsumer
	tl   *Timeline

	mu     sync.Mutex
	orders map[string]string
}

func NewRecordingConsumer(next ports.EventConsumer, tl *Timeline) *RecordingConsumer {
	return &RecordingConsumer{next: next, tl: tl, orders: map[string]string{}}
}

func (r *RecordingConsumer) Receive(ctx context.Context) (ports.Delivery, error) {
	d, err := r.next.Receive(ctx)
	if err != nil {
		return d, err
	}
	r.mu.Lock()
	r.orders[d.ID] = d.Event.OrderID
	r.mu.Unlock()
	r.tl.Add(d.Event.OrderID, "fila", Info, "worker recebeu o evento")
	return d, nil
}

func (r *RecordingConsumer) Ack(ctx context.Context, deliveryID string) error {
	if err := r.next.Ack(ctx, deliveryID); err != nil {
		return err
	}
	r.mu.Lock()
	orderID := r.orders[deliveryID]
	delete(r.orders, deliveryID)
	r.mu.Unlock()
	r.tl.Add(orderID, "fila", OK, "despacho concluído, evento confirmado")
	return nil
}

func (r *RecordingConsumer) Reject(ctx context.Context, d ports.Delivery, reason string, attempts int) error {
	if err := r.next.Reject(ctx, d, reason, attempts); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.orders, d.ID)
	r.mu.Unlock()
	r.tl.Add(d.Event.OrderID, "dlq", Error, fmt.Sprintf("enviado para a DLQ após %d tentativas: %s", attempts, reason))
	return nil
}

func (r *RecordingConsumer) DeadLetters(ctx context.Context) ([]ports.DeadLetter, error) {
	return r.next.DeadLetters(ctx)
}

func (r *RecordingConsumer) Discard(ctx context.Context, deadLetterID string) error {
	return r.next.Discard(ctx, deadLetterID)
}
