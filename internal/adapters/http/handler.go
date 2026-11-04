// Package httpadapter exposes the VTEX external seller protocol that the
// marketplace calls: simulation, order placement, fulfillment and cancellation.
package httpadapter

import (
	"context"
	"net/http"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type Simulator interface {
	Execute(ctx context.Context, items []domain.Item) (domain.Simulation, error)
}

type OrderPlacer interface {
	Execute(ctx context.Context, cmd app.PlaceOrderCommand) (*domain.Order, error)
}

type OrderAction interface {
	Execute(ctx context.Context, orderID string) error
}

type Deps struct {
	Simulate Simulator
	Place    OrderPlacer
	Fulfill  OrderAction
	Cancel   OrderAction
	Clock    ports.Clock
	// NewID generates seller order ids and receipts.
	NewID func() string
}

func NewHandler(d Deps) http.Handler {
	h := &handler{Deps: d}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /pvt/orderForms/simulation", h.simulate)
	mux.HandleFunc("POST /pvt/orders", h.placeOrder)
	mux.HandleFunc("POST /pvt/orders/{sellerOrderId}/fulfill", h.fulfill)
	mux.HandleFunc("POST /pvt/orders/{sellerOrderId}/cancel", h.cancel)
	return mux
}

type handler struct{ Deps }
