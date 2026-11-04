package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

var now = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func newOrder(t *testing.T) *domain.Order {
	t.Helper()
	o, err := domain.NewOrder("ord-1", "key-1", []domain.Item{{SKU: "sku-1", Quantity: 2}}, now)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

func TestNewOrderStartsReceived(t *testing.T) {
	o := newOrder(t)
	if o.Status != domain.StatusReceived {
		t.Fatalf("status = %s, want %s", o.Status, domain.StatusReceived)
	}
}

func TestNewOrderRejectsEmptyOrNonPositiveItems(t *testing.T) {
	cases := map[string][]domain.Item{
		"no items":      nil,
		"zero quantity": {{SKU: "a", Quantity: 0}},
		"empty sku":     {{SKU: "", Quantity: 1}},
	}
	for name, items := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := domain.NewOrder("o", "k", items, now)
			if !errors.Is(err, domain.ErrInvalidOrder) {
				t.Fatalf("err = %v, want ErrInvalidOrder", err)
			}
		})
	}
}

func TestAuthorizeTwiceIsRejectedWithTypedError(t *testing.T) {
	o := newOrder(t)
	if err := o.Authorize(); err != nil {
		t.Fatalf("first Authorize: %v", err)
	}
	err := o.Authorize()
	var te *domain.TransitionError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want *TransitionError", err)
	}
	if te.From != domain.StatusAuthorized || te.To != domain.StatusAuthorized {
		t.Fatalf("transition = %s -> %s", te.From, te.To)
	}
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("err should match ErrInvalidTransition")
	}
}

func TestHappyPathReachesShipped(t *testing.T) {
	o := newOrder(t)
	steps := []func() error{
		o.Authorize,
		o.MarkSentToERP,
		func() error { return o.MarkShipped("TRK-1") },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if o.Status != domain.StatusShipped || o.TrackingCode != "TRK-1" {
		t.Fatalf("got %s / %q", o.Status, o.TrackingCode)
	}
}

func TestInvalidTransitionsAreRejected(t *testing.T) {
	o := newOrder(t)
	if err := o.MarkSentToERP(); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("Received -> SentToERP: err = %v", err)
	}
	if err := o.MarkShipped("x"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("Received -> Shipped: err = %v", err)
	}
}

func TestFailAndCancelAreTerminal(t *testing.T) {
	for name, terminate := range map[string]func(*domain.Order) error{
		"fail":   (*domain.Order).Fail,
		"cancel": (*domain.Order).Cancel,
	} {
		t.Run(name, func(t *testing.T) {
			o := newOrder(t)
			if err := terminate(o); err != nil {
				t.Fatalf("terminate: %v", err)
			}
			if err := o.Authorize(); !errors.Is(err, domain.ErrInvalidTransition) {
				t.Fatalf("Authorize after terminal: err = %v", err)
			}
		})
	}
}

func TestShippedOrderCannotBeCancelled(t *testing.T) {
	o := newOrder(t)
	_ = o.Authorize()
	_ = o.MarkSentToERP()
	_ = o.MarkShipped("T")
	if err := o.Cancel(); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("err = %v", err)
	}
}
