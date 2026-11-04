package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/resilience"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/worker"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

const (
	maxQuantity     = 10
	maxStateOrders  = 12
	maxDemoBodySize = 4 << 10
	maxInvoices     = 5
	maxStateLetters = 20
	// replayTimeout stays well under the server's write timeout.
	replayTimeout = 20 * time.Second
)

var (
	idPattern  = regexp.MustCompile(`^[A-Za-z0-9-]{1,40}$`)
	skuPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,20}$`)
	// refPattern bounds the invoice and tracking numbers the simulator stores.
	refPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

type StockLine struct {
	SKU        string `json:"sku"`
	Name       string `json:"name"`
	PriceCents int64  `json:"priceCents"`
	Available  int    `json:"available"`
}

type Deps struct {
	// Seller is the protocol handler the marketplace calls, invoked in
	// process so the demo does not depend on its own rate limit.
	Seller http.Handler
	// Endpoint is the marketplaceServicesEndpoint sent with each placement:
	// the address of the invoice receiver below.
	Endpoint string
	Orders   interface {
		FindByIdempotencyKey(ctx context.Context, key string) (*domain.Order, error)
	}
	Timeline *Timeline
	Breaker  interface{ Snapshot() resilience.Snapshot }
	// BreakerOpenTimeout lets the panel show how long until the probe.
	BreakerOpenTimeout time.Duration
	// DeadLetters returns the newest letters, up to limit, and the DLQ total.
	DeadLetters   func(ctx context.Context, limit int) ([]ports.DeadLetter, int64, error)
	Replay        func(ctx context.Context) (worker.ReplayResult, error)
	Stock         func() []StockLine
	SetERPFailing func(ctx context.Context, failing bool) error
	// Resets clear the state that lives outside the controller.
	Resets []func(ctx context.Context) error
	// ResetERP clears the ERP mock; the panel's ERP flag only goes back to
	// "up" when it succeeds. Nil means there is nothing to reset.
	ResetERP func(ctx context.Context) error
	// InternalOnly decides whether a request came from the internal network;
	// the invoice endpoint refuses everyone else. A nil value refuses all.
	InternalOnly func(*http.Request) bool
	NewID        func() string
	Now          func() time.Time
}

// Controller plays the marketplace: it sends simulation, placement and
// fulfillment to the seller, receives the invoice, and exposes the state the
// panel polls.
type Controller struct {
	Deps

	mu         sync.Mutex
	invoices   map[string]map[string]bool
	erpFailing bool
	resetMu    sync.Mutex
}

func NewController(d Deps) *Controller {
	return &Controller{Deps: d, invoices: map[string]map[string]bool{}}
}

func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /demo/orders", c.createOrder)
	mux.HandleFunc("POST /demo/orders/{marketplaceOrderId}/fulfill", c.fulfill)
	mux.HandleFunc("POST /demo/mkt/pvt/orders/{marketplaceOrderId}/invoice", c.receiveInvoice)
	mux.HandleFunc("GET /demo/state", c.state)
	mux.HandleFunc("POST /demo/erp", c.switchERP)
	mux.HandleFunc("POST /demo/dlq/replay", c.replay)
	mux.HandleFunc("POST /demo/reset", c.reset)
	return mux
}

// Reset clears the demo: orders, queues, breaker, stock and the mocks.
func (c *Controller) Reset(ctx context.Context) error {
	c.resetMu.Lock()
	defer c.resetMu.Unlock()
	var errs []error
	for _, step := range c.Resets {
		if err := step(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	var erpErr error
	if c.ResetERP != nil {
		erpErr = c.ResetERP(ctx)
		errs = append(errs, erpErr)
	}
	c.Timeline.Reset()
	c.mu.Lock()
	c.invoices = map[string]map[string]bool{}
	if erpErr == nil {
		c.erpFailing = false
	}
	c.mu.Unlock()
	return errors.Join(errs...)
}

type orderRequest struct {
	MarketplaceOrderID string `json:"marketplaceOrderId"`
	SKU                string `json:"sku"`
	Quantity           int    `json:"quantity"`
}

func (c *Controller) createOrder(w http.ResponseWriter, r *http.Request) {
	req := orderRequest{SKU: "6", Quantity: 1}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.MarketplaceOrderID == "" {
		req.MarketplaceOrderID = "DEMO-" + c.NewID()
	}
	if !idPattern.MatchString(req.MarketplaceOrderID) || !skuPattern.MatchString(req.SKU) ||
		req.Quantity < 1 || req.Quantity > maxQuantity {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("marketplaceOrderId and sku must be short alphanumeric values and quantity between 1 and %d", maxQuantity))
		return
	}
	mid, ctx := req.MarketplaceOrderID, r.Context()

	_, duplicate := c.Timeline.SellerOrderID(mid)
	price, freight, simulation, err := c.simulate(ctx, req)
	if err != nil {
		slog.Error("demo: simulation failed", "error", err)
		writeError(w, http.StatusBadGateway, "simulation_failed", "the seller did not answer the simulation")
		return
	}
	status, body, err := c.callSeller(ctx, http.MethodPost, "/pvt/orders", map[string]any{
		"marketplaceOrderId":          mid,
		"marketplaceServicesEndpoint": c.Endpoint,
		"items":                       []map[string]any{{"id": req.SKU, "quantity": req.Quantity, "seller": "1", "price": price}},
		"shippingData":                map[string]any{"logisticsInfo": []map[string]any{{"itemIndex": 0, "price": freight}}},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	if status == http.StatusConflict {
		c.Timeline.Add(mid, "marketplace", Info, simulation)
		c.Timeline.Add(mid, "marketplace", Error, "seller recusou o pedido: sem estoque suficiente")
		writeError(w, http.StatusConflict, "insufficient_stock", "not enough stock to place the order")
		return
	}
	var placed struct {
		OrderID string `json:"orderId"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &placed) != nil || placed.OrderID == "" {
		slog.Error("demo: placement failed", "status", status)
		writeError(w, http.StatusBadGateway, "placement_failed", "the seller refused the placement")
		return
	}

	// The placement recorder already linked the ids and noted the placement;
	// the simulation came first, so it goes in front, and only now that the
	// placement worked (a failed simulation or placement leaves no entry).
	if !duplicate {
		c.Timeline.Prepend(mid, "marketplace", Info, simulation)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"marketplaceOrderId": mid, "sellerOrderId": placed.OrderID, "duplicate": duplicate,
	})
}

// simulate asks the seller for price and stock, as the marketplace does
// before placing an order, and returns the unit price and the freight quoted.
func (c *Controller) simulate(ctx context.Context, req orderRequest) (int64, int64, string, error) {
	status, body, err := c.callSeller(ctx, http.MethodPost, "/pvt/orderForms/simulation", map[string]any{
		"postalCode": "01310-100", "country": "BRA",
		"items": []map[string]any{{"id": req.SKU, "quantity": req.Quantity, "seller": "1"}},
	})
	if err != nil {
		return 0, 0, "", err
	}
	var sim struct {
		Items         []struct{ Price int64 } `json:"items"`
		LogisticsInfo []struct {
			SLAs []struct{ Price int64 } `json:"slas"`
		} `json:"logisticsInfo"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &sim) != nil || len(sim.Items) == 0 || len(sim.LogisticsInfo) == 0 {
		return 0, 0, "", fmt.Errorf("simulation answered status %d", status)
	}
	message, freight := "simulação: sem estoque suficiente para este SKU", int64(0)
	if len(sim.LogisticsInfo[0].SLAs) > 0 {
		freight = sim.LogisticsInfo[0].SLAs[0].Price
		message = fmt.Sprintf("simulação: preço %s, frete %s", money(sim.Items[0].Price), money(freight))
	}
	return sim.Items[0].Price, freight, message, nil
}

func (c *Controller) fulfill(w http.ResponseWriter, r *http.Request) {
	mid, ctx := r.PathValue("marketplaceOrderId"), r.Context()
	sellerID, ok := c.Timeline.SellerOrderID(mid)
	if !ok {
		order, err := c.Orders.FindByIdempotencyKey(ctx, mid)
		if err != nil {
			writeError(w, http.StatusNotFound, "order_not_found", "order not found")
			return
		}
		sellerID = order.ID
	}
	// Recorded before the call: the seller publishes the event inside it, and
	// the timeline must read in causal order.
	c.Timeline.Add(mid, "marketplace", Info, "pagamento aprovado: marketplace autorizou o despacho (fulfill)")
	status, _, err := c.callSeller(ctx, http.MethodPost, "/pvt/orders/"+sellerID+"/fulfill", map[string]any{"marketplaceOrderId": mid})
	if err != nil || status != http.StatusOK {
		c.Timeline.Add(mid, "marketplace", Error, "seller recusou o despacho")
		slog.Error("demo: fulfill failed", "status", status, "error", err)
		writeError(w, http.StatusBadGateway, "fulfill_failed", "the seller refused the fulfillment")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"marketplaceOrderId": mid, "sellerOrderId": sellerID})
}

// receiveInvoice plays the marketplace endpoint that the seller calls. A
// repeated invoice number is answered with 409, as the real one does.
func (c *Controller) receiveInvoice(w http.ResponseWriter, r *http.Request) {
	if c.InternalOnly == nil || !c.InternalOnly(r) {
		writeError(w, http.StatusForbidden, "forbidden", "the invoice endpoint only accepts calls from the internal network")
		return
	}
	mid := r.PathValue("marketplaceOrderId")
	if len(c.Timeline.Events(mid)) == 0 {
		writeError(w, http.StatusNotFound, "order_not_found", "unknown marketplace order")
		return
	}
	var inv struct {
		InvoiceNumber  string `json:"invoiceNumber"`
		TrackingNumber string `json:"trackingNumber"`
		InvoiceValue   int64  `json:"invoiceValue"`
	}
	if !decodeBody(w, r, &inv) {
		return
	}
	if !refPattern.MatchString(inv.InvoiceNumber) || (inv.TrackingNumber != "" && !refPattern.MatchString(inv.TrackingNumber)) {
		writeError(w, http.StatusBadRequest, "invalid_request", "invoiceNumber and trackingNumber must be 1 to 64 letters, digits or hyphens")
		return
	}
	c.mu.Lock()
	seen := c.invoices[mid][inv.InvoiceNumber]
	if !seen && len(c.invoices[mid]) >= maxInvoices {
		c.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "invoice_limit", "too many distinct invoices for this order")
		return
	}
	if c.invoices[mid] == nil {
		c.invoices[mid] = map[string]bool{}
	}
	c.invoices[mid][inv.InvoiceNumber] = true
	c.mu.Unlock()
	if seen {
		c.Timeline.Add(mid, "marketplace", Info, "nota repetida ignorada (409), sem duplicar")
		writeError(w, http.StatusConflict, "invoice_exists", "invoice already received")
		return
	}
	c.Timeline.Add(mid, "marketplace", OK, fmt.Sprintf("marketplace recebeu a nota %s com o rastreio %s, valor %s", inv.InvoiceNumber, inv.TrackingNumber, money(inv.InvoiceValue)))
	writeJSON(w, http.StatusOK, map[string]string{"invoiceNumber": inv.InvoiceNumber})
}

type orderView struct {
	MarketplaceOrderID string  `json:"marketplaceOrderId"`
	SellerOrderID      string  `json:"sellerOrderId,omitempty"`
	Status             string  `json:"status"`
	TrackingCode       string  `json:"trackingCode,omitempty"`
	Events             []Event `json:"events"`
}

type letterView struct {
	ID       string    `json:"id"`
	OrderID  string    `json:"orderId"`
	Type     string    `json:"type"`
	Reason   string    `json:"reason"`
	Attempts int       `json:"attempts"`
	FailedAt time.Time `json:"failedAt"`
}

type breakerView struct {
	State               resilience.State `json:"state"`
	ConsecutiveFailures int              `json:"consecutiveFailures"`
	OpenTimeoutMs       int64            `json:"openTimeoutMs"`
	RetryInMs           int64            `json:"retryInMs"`
}

func (c *Controller) state(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orders := []orderView{}
	for _, mid := range c.Timeline.Orders() {
		if len(orders) == maxStateOrders {
			break
		}
		v := orderView{MarketplaceOrderID: mid, Status: "not_created", Events: c.Timeline.Events(mid)}
		if order, err := c.Orders.FindByIdempotencyKey(ctx, mid); err == nil {
			v.SellerOrderID, v.Status, v.TrackingCode = order.ID, string(order.Status), order.TrackingCode
		}
		orders = append(orders, v)
	}
	letters, total, err := c.DeadLetters(ctx, maxStateLetters)
	if err != nil {
		slog.Error("demo: reading the DLQ failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	dlq := make([]letterView, len(letters))
	for i, l := range letters {
		dlq[i] = letterView{ID: l.ID, OrderID: l.Event.OrderID, Type: string(l.Event.Type), Reason: l.Reason, Attempts: l.Attempts, FailedAt: l.FailedAt}
	}
	snap := c.Breaker.Snapshot()
	bv := breakerView{State: snap.State, ConsecutiveFailures: snap.ConsecutiveFailures, OpenTimeoutMs: c.BreakerOpenTimeout.Milliseconds()}
	if snap.State == resilience.StateOpen {
		bv.RetryInMs = max(0, snap.OpenedAt.Add(c.BreakerOpenTimeout).Sub(c.Now()).Milliseconds())
	}
	c.mu.Lock()
	failing := c.erpFailing
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"serverTime": c.Now(), "orders": orders, "dlq": dlq, "dlqTotal": total, "breaker": bv,
		"erp": map[string]bool{"failing": failing}, "stock": c.Stock(),
	})
}

func (c *Controller) switchERP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Failing bool `json:"failing"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	c.resetMu.Lock()
	defer c.resetMu.Unlock()
	if err := c.SetERPFailing(r.Context(), req.Failing); err != nil {
		slog.Error("demo: switching the ERP failed", "error", err)
		writeError(w, http.StatusBadGateway, "erp_unreachable", "could not reach the ERP mock")
		return
	}
	c.mu.Lock()
	c.erpFailing = req.Failing
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"failing": req.Failing})
}

func (c *Controller) replay(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), replayTimeout)
	defer cancel()
	res, err := c.Replay(ctx)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		slog.Error("demo: DLQ replay failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"replayed": res.Replayed, "failed": res.Failed, "discarded": res.Discarded})
}

func (c *Controller) reset(w http.ResponseWriter, r *http.Request) {
	if err := c.Reset(r.Context()); err != nil {
		slog.Error("demo: reset failed", "error", err)
		writeError(w, http.StatusInternalServerError, "reset_failed", "reset was incomplete")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"reset": true})
}

func (c *Controller) callSeller(ctx context.Context, method, path string, body any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://seller.internal"+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c.Seller.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), nil
}

func money(cents int64) string { return fmt.Sprintf("R$ %d,%02d", cents/100, cents%100) }

// decodeBody accepts an empty body, which leaves v at its defaults.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDemoBodySize)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]map[string]string{"error": {"code": code, "message": message}})
}
