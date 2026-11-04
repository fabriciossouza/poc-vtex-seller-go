// Package demo is the marketplace simulator and the control surface of the
// public demo: it drives the scenarios, receives the invoice the way the
// marketplace would, and keeps a timeline of what happened to each order.
package demo

import (
	"sync"
	"time"
)

type Level string

const (
	Info  Level = "info"
	OK    Level = "ok"
	Error Level = "error"
)

const maxMessageRunes = 240

type Event struct {
	At      time.Time `json:"at"`
	Source  string    `json:"source"`
	Level   Level     `json:"level"`
	Message string    `json:"message"`
}

// Timeline keeps the events of the most recent orders, keyed by marketplace
// order id. Both ids of an order resolve to the same entry once aliased.
type Timeline struct {
	now       func() time.Time
	maxOrders int
	maxEvents int

	mu      sync.Mutex
	events  map[string][]Event
	recency []string // oldest first
	toMkt   map[string]string
	toSell  map[string]string
}

func NewTimeline(now func() time.Time, maxOrders, maxEvents int) *Timeline {
	t := &Timeline{now: now, maxOrders: maxOrders, maxEvents: maxEvents}
	t.Reset()
	return t
}

func (t *Timeline) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = map[string][]Event{}
	t.recency = nil
	t.toMkt = map[string]string{}
	t.toSell = map[string]string{}
}

// Alias links the seller order id to the marketplace order id.
func (t *Timeline) Alias(sellerOrderID, marketplaceOrderID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.toMkt[sellerOrderID] = marketplaceOrderID
	t.toSell[marketplaceOrderID] = sellerOrderID
}

func (t *Timeline) SellerOrderID(marketplaceOrderID string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.toSell[marketplaceOrderID]
	return id, ok
}

// Add records an event under a marketplace or seller order id.
func (t *Timeline) Add(orderRef, source string, level Level, message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := orderRef
	if mkt, ok := t.toMkt[orderRef]; ok {
		id = mkt
	}
	if _, known := t.events[id]; !known {
		t.recency = append(t.recency, id)
		t.evict()
	}
	events := append(t.events[id], Event{At: t.now(), Source: source, Level: level, Message: truncate(message)})
	if len(events) > t.maxEvents {
		events = events[len(events)-t.maxEvents:]
	}
	t.events[id] = events
}

func (t *Timeline) evict() {
	for len(t.recency) > t.maxOrders {
		oldest := t.recency[0]
		t.recency = t.recency[1:]
		delete(t.events, oldest)
		if seller, ok := t.toSell[oldest]; ok {
			delete(t.toMkt, seller)
			delete(t.toSell, oldest)
		}
	}
}

func (t *Timeline) Events(marketplaceOrderID string) []Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Event(nil), t.events[marketplaceOrderID]...)
}

// Orders lists marketplace order ids, newest first.
func (t *Timeline) Orders() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.recency))
	for i, id := range t.recency {
		out[len(out)-1-i] = id
	}
	return out
}

func truncate(s string) string {
	runes := []rune(s)
	if len(runes) <= maxMessageRunes {
		return s
	}
	return string(runes[:maxMessageRunes]) + "..."
}
