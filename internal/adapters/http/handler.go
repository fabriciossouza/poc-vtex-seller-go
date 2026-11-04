// Package httpadapter exposes the VTEX external seller protocol that the
// marketplace calls: simulation, order placement, fulfillment and cancellation.
package httpadapter

import (
	"context"
	"net/http"
	"regexp"
	"time"

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
	Execute(ctx context.Context, orderID, marketplaceOrderID string) error
}

type Deps struct {
	Simulate Simulator
	Place    OrderPlacer
	Fulfill  OrderAction
	Cancel   OrderAction
	// ValidateEndpoint vets the marketplaceServicesEndpoint of a placement; the
	// value is caller-controlled, so a nil validator refuses every placement.
	ValidateEndpoint func(endpoint string) error
	Clock            ports.Clock
	// SimulationTimeout bounds one simulation; zero means 2s.
	SimulationTimeout time.Duration
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

var (
	// idPattern bounds ids that come from the caller: they end up in logs,
	// timeline entries and URLs, so dots and slashes are refused.
	idPattern  = regexp.MustCompile(`^[A-Za-z0-9-]{1,40}$`)
	skuPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,20}$`)
)

// maxItems bounds the work one request can ask for.
const maxItems = 50

type handler struct{ Deps }
