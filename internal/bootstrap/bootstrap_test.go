package bootstrap_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/bootstrap"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/resilience"
)

// erpStub behaves like the PHP mock: idempotent by Idempotency-Key, with a
// switch that makes it answer 503.
type erpStub struct {
	mu         sync.Mutex
	resetFails bool
	failing    bool
	orders     map[string]bool
	resets     int
}

func (e *erpStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch r.URL.Path {
	case "/orders":
		if e.failing {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		e.orders[r.Header.Get("Idempotency-Key")] = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"registered"}`))
	case "/admin/config":
		var cfg struct {
			ErrorRate float64 `json:"errorRate"`
		}
		_ = json.NewDecoder(r.Body).Decode(&cfg)
		e.failing = cfg.ErrorRate >= 1
	case "/admin/reset":
		if e.resetFails {
			http.Error(w, "reset broken", http.StatusInternalServerError)
			return
		}
		e.resets++
		e.failing = false
		e.orders = map[string]bool{}
	default:
		http.NotFound(w, r)
	}
}

func (e *erpStub) registered() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.orders)
}

type tmsStub struct {
	mu         sync.Mutex
	resets     int
	quotesFail bool
}

func (s *tmsStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/quotes":
		if s.quotesFail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"priceCents":1099,"estimateHours":168}`))
	case "/shipments":
		_, _ = w.Write([]byte(`{"trackingCode":"TRK-TEST1"}`))
	case "/admin/reset":
		s.resets++
	default:
		http.NotFound(w, r)
	}
}

type world struct {
	t       *testing.T
	rdb     *redis.Client
	url     string
	erp     *erpStub
	tms     *tmsStub
	app     *bootstrap.App
	handler http.Handler
}

type options struct {
	rateBurst     int
	ratePerSecond float64
}

func start(t *testing.T, opts ...func(*options)) *world {
	t.Helper()
	o := options{rateBurst: 1000, ratePerSecond: 1000}
	for _, f := range opts {
		f(&o)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	erp, tms := &erpStub{orders: map[string]bool{}}, &tmsStub{}
	erpSrv, tmsSrv := httptest.NewServer(erp), httptest.NewServer(tms)
	t.Cleanup(erpSrv.Close)
	t.Cleanup(tmsSrv.Close)

	// The seller calls the simulator back, so the app needs a real listener
	// whose address is known before the app is built.
	srv := httptest.NewUnstartedServer(nil)
	host := srv.Listener.Addr().String()

	app, err := bootstrap.New(context.Background(), bootstrap.Config{
		Redis:                   rdb,
		DBPath:                  t.TempDir() + "/seller.db",
		ERPURL:                  erpSrv.URL,
		TMSURL:                  tmsSrv.URL,
		MarketplaceAllowedHosts: []string{host},
		MarketplaceEndpoint:     "http://" + host + "/demo/mkt",
		DependencyTimeout:       2 * time.Second,
		Retry:                   resilience.RetryConfig{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
		Breaker:                 resilience.BreakerConfig{FailureThreshold: 3, OpenTimeout: 150 * time.Millisecond},
		QueueBlock:              10 * time.Millisecond,
		RateLimitPerSecond:      o.ratePerSecond,
		RateLimitBurst:          o.rateBurst,
		InternalNets:            []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")},
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	srv.Config.Handler = app.Handler
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = app.Worker.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; app.Close() })
	return &world{t: t, rdb: rdb, url: srv.URL, erp: erp, tms: tms, app: app, handler: app.Handler}
}

func (w *world) do(method, path string, body any) (int, map[string]any) {
	w.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, w.url+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (w *world) state() map[string]any {
	w.t.Helper()
	status, st := w.do("GET", "/demo/state", nil)
	if status != http.StatusOK {
		w.t.Fatalf("state status = %d", status)
	}
	return st
}

func (w *world) orderState(mid string) map[string]any {
	for _, o := range w.state()["orders"].([]any) {
		if o.(map[string]any)["marketplaceOrderId"] == mid {
			return o.(map[string]any)
		}
	}
	return nil
}

func (w *world) eventually(what string, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.t.Fatalf("timed out waiting for %s; state: %v", what, w.state())
}

func (w *world) place(mid string) map[string]any {
	w.t.Helper()
	status, got := w.do("POST", "/demo/orders", map[string]any{"marketplaceOrderId": mid, "sku": "6", "quantity": 2})
	if status != http.StatusOK {
		w.t.Fatalf("place status = %d, body %v", status, got)
	}
	return got
}

func (w *world) fulfill(mid string) {
	w.t.Helper()
	if status, got := w.do("POST", "/demo/orders/"+mid+"/fulfill", nil); status != http.StatusOK {
		w.t.Fatalf("fulfill status = %d, body %v", status, got)
	}
}

func stockOf(st map[string]any, sku string) float64 {
	for _, s := range st["stock"].([]any) {
		if s.(map[string]any)["sku"] == sku {
			return s.(map[string]any)["available"].(float64)
		}
	}
	return -1
}

func sources(order map[string]any) string {
	var b strings.Builder
	for _, e := range order["events"].([]any) {
		ev := e.(map[string]any)
		b.WriteString(ev["source"].(string) + ":" + ev["level"].(string) + ":" + ev["message"].(string) + "\n")
	}
	return b.String()
}

func TestHappyPathRunsFromPlacementToTrackingAndTheMarketplaceReceivesTheInvoice(t *testing.T) {
	w := start(t)

	placed := w.place("HAPPY-1")
	w.fulfill("HAPPY-1")

	w.eventually("order shipped and invoice received", func() bool {
		o := w.orderState("HAPPY-1")
		return o != nil && o["status"] == "shipped" && strings.Contains(sources(o), "marketplace:ok:marketplace recebeu a nota")
	})
	o := w.orderState("HAPPY-1")
	if o["trackingCode"] != "TRK-TEST1" || o["sellerOrderId"] != placed["sellerOrderId"] {
		t.Fatalf("order = %v", o)
	}
	for _, want := range []string{"marketplace:", "erp:ok", "tms:ok", "fila:ok"} {
		if !strings.Contains(sources(o), want) {
			t.Errorf("timeline lacks %q:\n%s", want, sources(o))
		}
	}
}

func TestHappyPathTimelineReadsInCausalOrderWithAccents(t *testing.T) {
	w := start(t)
	w.place("CAUSAL-1")
	w.fulfill("CAUSAL-1")
	w.eventually("invoice received and delivery settled", func() bool {
		o := w.orderState("CAUSAL-1")
		return o != nil && strings.Contains(sources(o), "despacho concluído")
	})

	var messages []string
	for _, e := range w.orderState("CAUSAL-1")["events"].([]any) {
		messages = append(messages, e.(map[string]any)["message"].(string))
	}
	steps := []string{
		"simulação", "pedido enviado", "autorizou o despacho", "publica o evento order.authorized",
		"worker recebeu o evento", "ERP registrou", "TMS criou a remessa", "seller envia a nota",
		"marketplace recebeu a nota", "despacho concluído",
	}
	prev := -1
	for _, step := range steps {
		at := -1
		for i, m := range messages {
			if strings.Contains(m, step) {
				at = i
				break
			}
		}
		if at <= prev {
			t.Fatalf("%q is missing or out of order:\n%s", step, strings.Join(messages, "\n"))
		}
		prev = at
	}
	for _, ascii := range []string{"simulacao", " nao ", "concluido", " apos ", "preco"} {
		if strings.Contains(strings.Join(messages, "\n"), ascii) {
			t.Errorf("message without accent: %q", ascii)
		}
	}
}

func TestDuplicatePlacementYieldsASingleOrderAndASingleStockReservation(t *testing.T) {
	w := start(t)
	before := stockOf(w.state(), "6")

	first := w.place("DUP-1")
	second := w.place("DUP-1")

	if second["sellerOrderId"] != first["sellerOrderId"] || second["duplicate"] != true || first["duplicate"] == true {
		t.Fatalf("first = %v, second = %v", first, second)
	}
	st := w.state()
	if n := len(st["orders"].([]any)); n != 1 {
		t.Fatalf("orders = %d, want 1", n)
	}
	if after := stockOf(st, "6"); after != before-2 {
		t.Fatalf("stock = %v, want %v (reserved once)", after, before-2)
	}
}

func TestERPDownRetriesOpensTheBreakerAndParksTheOrderInTheDLQ(t *testing.T) {
	w := start(t)
	if status, _ := w.do("POST", "/demo/erp", map[string]any{"failing": true}); status != http.StatusOK {
		t.Fatalf("erp switch status = %d", status)
	}

	w.place("DOWN-1")
	w.fulfill("DOWN-1")

	w.eventually("letter in the DLQ", func() bool { return len(w.state()["dlq"].([]any)) == 1 })
	st := w.state()
	letter := st["dlq"].([]any)[0].(map[string]any)
	if !strings.Contains(letter["reason"].(string), "circuit breaker is open") || letter["attempts"].(float64) < 3 {
		t.Fatalf("letter = %v", letter)
	}
	if st["breaker"].(map[string]any)["state"] != "open" {
		t.Fatalf("breaker = %v", st["breaker"])
	}
	if st["erp"].(map[string]any)["failing"] != true {
		t.Fatalf("erp = %v", st["erp"])
	}
	if !strings.Contains(sources(w.orderState("DOWN-1")), "dlq:error:") {
		t.Fatalf("timeline lacks the DLQ event:\n%s", sources(w.orderState("DOWN-1")))
	}
}

func TestRestoringTheERPAndReplayingTheDLQCompletesTheOrderExactlyOnceInTheERP(t *testing.T) {
	w := start(t)
	w.do("POST", "/demo/erp", map[string]any{"failing": true})
	w.place("REPLAY-1")
	w.fulfill("REPLAY-1")
	w.eventually("letter in the DLQ", func() bool { return len(w.state()["dlq"].([]any)) == 1 })

	w.do("POST", "/demo/erp", map[string]any{"failing": false})
	w.eventually("breaker half-open", func() bool {
		return w.state()["breaker"].(map[string]any)["state"] != "open"
	})
	status, res := w.do("POST", "/demo/dlq/replay", nil)

	if status != http.StatusOK || res["replayed"] != float64(1) {
		t.Fatalf("replay = %d %v", status, res)
	}
	w.eventually("order shipped", func() bool { return w.orderState("REPLAY-1")["status"] == "shipped" })
	st := w.state()
	if len(st["dlq"].([]any)) != 0 || st["breaker"].(map[string]any)["state"] != "closed" {
		t.Fatalf("dlq = %v, breaker = %v", st["dlq"], st["breaker"])
	}
	if w.erp.registered() != 1 {
		t.Fatalf("ERP orders = %d, want 1", w.erp.registered())
	}
}

func TestResetClearsOrdersDLQBreakerStockAndTheMocks(t *testing.T) {
	w := start(t)
	w.do("POST", "/demo/erp", map[string]any{"failing": true})
	w.place("RESET-1")
	w.fulfill("RESET-1")
	w.eventually("letter in the DLQ", func() bool { return len(w.state()["dlq"].([]any)) == 1 })

	if status, _ := w.do("POST", "/demo/reset", nil); status != http.StatusOK {
		t.Fatalf("reset status = %d", status)
	}

	st := w.state()
	if len(st["orders"].([]any)) != 0 || len(st["dlq"].([]any)) != 0 {
		t.Fatalf("state after reset = %v", st)
	}
	if st["breaker"].(map[string]any)["state"] != "closed" || st["erp"].(map[string]any)["failing"] != false {
		t.Fatalf("breaker/erp after reset = %v / %v", st["breaker"], st["erp"])
	}
	if stockOf(st, "6") != 50 {
		t.Fatalf("stock = %v, want the initial 50", stockOf(st, "6"))
	}
	if w.erp.resets != 1 || w.tms.resets != 1 {
		t.Fatalf("mock resets = %d / %d", w.erp.resets, w.tms.resets)
	}
	w.place("RESET-1") // the key is free again
}

func TestPlacementWithoutStockIsRefusedWithAClearStatus(t *testing.T) {
	w := start(t)

	status, got := w.do("POST", "/demo/orders", map[string]any{"sku": "9", "quantity": 1})

	if status != http.StatusConflict {
		t.Fatalf("status = %d, body %v", status, got)
	}
	for _, o := range w.state()["orders"].([]any) {
		if o.(map[string]any)["sellerOrderId"] != nil || o.(map[string]any)["status"] != "not_created" {
			t.Fatalf("a refused placement must not create an order: %v", o)
		}
	}
}

func TestDemoInputIsValidated(t *testing.T) {
	w := start(t)
	for name, body := range map[string]map[string]any{
		"quantity zero":  {"sku": "6", "quantity": 0},
		"quantity large": {"sku": "6", "quantity": 1000},
		"odd id":         {"marketplaceOrderId": "../etc", "sku": "6", "quantity": 1},
		"odd sku":        {"sku": "<script>", "quantity": 1},
	} {
		if status, _ := w.do("POST", "/demo/orders", body); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, status)
		}
	}
	if status, _ := w.do("POST", "/demo/orders/unknown/fulfill", nil); status != http.StatusNotFound {
		t.Errorf("fulfill unknown: status = %d, want 404", status)
	}
}

func TestSimulatorInvoiceEndpointRejectsUnknownOrdersAndBadBodies(t *testing.T) {
	w := start(t)

	if status, _ := w.do("POST", "/demo/mkt/pvt/orders/NOPE/invoice", map[string]any{"type": "Output", "invoiceNumber": "INV-1"}); status != http.StatusNotFound {
		t.Errorf("unknown order: status = %d, want 404", status)
	}
	w.place("INV-1")
	req, _ := http.NewRequest("POST", w.url+"/demo/mkt/pvt/orders/INV-1/invoice", strings.NewReader("{not json"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad body: status = %d, want 400", resp.StatusCode)
	}
}

func TestRepeatedInvoiceIsAnsweredWith409LikeTheRealMarketplace(t *testing.T) {
	w := start(t)
	w.place("INV-2")
	body := map[string]any{"type": "Output", "invoiceNumber": "INV-X", "trackingNumber": "T"}

	first, _ := w.do("POST", "/demo/mkt/pvt/orders/INV-2/invoice", body)
	second, _ := w.do("POST", "/demo/mkt/pvt/orders/INV-2/invoice", body)

	if first != http.StatusOK || second != http.StatusConflict {
		t.Fatalf("statuses = %d, %d; want 200 then 409", first, second)
	}
}

func TestHealthMetricsAndSecurityHeaders(t *testing.T) {
	w := start(t)
	w.do("GET", "/demo/state", nil)

	req, _ := http.NewRequest("GET", w.url+"/healthz", nil)
	health, err := http.DefaultClient.Do(req)
	if err != nil || health.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %v, %v", health, err)
	}
	_ = health.Body.Close()
	if health.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(health.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("security headers = %v", health.Header)
	}

	resp, err := http.Get(w.url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"http_requests_total", "breaker_open", "dlq_depth"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}

func TestExceedingTheRateLimitAnswers429(t *testing.T) {
	w := start(t, func(o *options) { o.rateBurst, o.ratePerSecond = 3, 0.01 })

	var last int
	for i := 0; i < 8; i++ {
		last, _ = w.do("GET", "/demo/state", nil)
	}

	if last != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", last)
	}
}

func TestAppServesThePanelTheDocsAndTheSpec(t *testing.T) {
	w := start(t)
	for path, want := range map[string]string{"/": "btn-happy", "/docs/": "swagger-ui", "/docs/openapi.yaml": "openapi: 3."} {
		resp, err := http.Get(w.url + path)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), want) {
			t.Errorf("%s: status %d, missing %q", path, resp.StatusCode, want)
		}
	}
}

func (w *world) invoiceFrom(remote, xff, mid string, body map[string]any) int {
	w.t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/demo/mkt/pvt/orders/"+mid+"/invoice", bytes.NewReader(raw))
	req.RemoteAddr = remote
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, req)
	return rec.Code
}

func TestInvoiceEndpointAcceptsOnlyTheInternalNetwork(t *testing.T) {
	w := start(t)
	w.place("NET-1")
	body := map[string]any{"type": "Output", "invoiceNumber": "INV-N", "trackingNumber": "T-1"}

	if code := w.invoiceFrom("203.0.113.7:1", "", "NET-1", body); code != http.StatusForbidden {
		t.Errorf("public peer: status = %d, want 403", code)
	}
	if code := w.invoiceFrom("127.0.0.1:1", "198.51.100.1", "NET-1", body); code != http.StatusForbidden {
		t.Errorf("request through the proxy: status = %d, want 403", code)
	}
	if code := w.invoiceFrom("127.0.0.1:1", "", "NET-1", body); code != http.StatusOK {
		t.Errorf("internal peer: status = %d, want 200", code)
	}
}

func TestInvoiceFieldsAndCountPerOrderAreBounded(t *testing.T) {
	w := start(t)
	w.place("LIM-1")
	call := func(invoice, tracking string) int {
		return w.invoiceFrom("127.0.0.1:1", "", "LIM-1", map[string]any{"type": "Output", "invoiceNumber": invoice, "trackingNumber": tracking})
	}

	for name, args := range map[string][2]string{
		"invoice with slash": {"a/b", "T"},
		"invoice too long":   {strings.Repeat("a", 65), "T"},
		"tracking with html": {"INV-OK", "<b>"},
		"tracking too long":  {"INV-OK", strings.Repeat("9", 65)},
	} {
		if code := call(args[0], args[1]); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, code)
		}
	}
	for i := 0; i < 5; i++ {
		if code := call("INV-"+strings.Repeat("x", i+1), "T"); code != http.StatusOK {
			t.Fatalf("invoice %d: status = %d, want 200", i+1, code)
		}
	}
	if code := call("INV-one-too-many", "T"); code != http.StatusTooManyRequests {
		t.Errorf("sixth distinct invoice: status = %d, want 429", code)
	}
}

func (w *world) addLetters(n int, orderPrefix string) {
	w.t.Helper()
	for i := 0; i < n; i++ {
		err := w.rdb.XAdd(context.Background(), &redis.XAddArgs{Stream: "orders.dlq", Values: map[string]any{
			"type": "order.authorized", "order_id": fmt.Sprintf("%s-%02d", orderPrefix, i), "reason": "x",
			"attempts": 4, "failed_at": time.Now().UTC().Format(time.RFC3339Nano), "source_id": "0-1",
		}}).Err()
		if err != nil {
			w.t.Fatal(err)
		}
	}
}

func TestStateShowsTheTwentyNewestLettersWithTheTotalAndMetricsUseXLEN(t *testing.T) {
	w := start(t)
	w.addLetters(25, "ghost")

	st := w.state()

	letters := st["dlq"].([]any)
	if len(letters) != 20 || st["dlqTotal"] != float64(25) {
		t.Fatalf("letters = %d, dlqTotal = %v; want 20 and 25", len(letters), st["dlqTotal"])
	}
	if first := letters[0].(map[string]any)["orderId"]; first != "ghost-24" {
		t.Fatalf("first letter = %v, want the newest (ghost-24)", first)
	}
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "dlq_depth 25") {
		t.Fatalf("metrics lack dlq_depth 25:\n%s", rec.Body.String())
	}
}

func TestReplayHandlesAtMostOneBatchPerCall(t *testing.T) {
	w := start(t)
	w.addLetters(25, "ghost")

	status, res := w.do("POST", "/demo/dlq/replay", nil)

	if status != http.StatusOK || res["discarded"] != float64(20) {
		t.Fatalf("status = %d, res = %v; want 20 discarded in this call", status, res)
	}
	if total := w.state()["dlqTotal"]; total != float64(5) {
		t.Fatalf("dlqTotal = %v, want 5 left for the next call", total)
	}
}

func (w *world) sellerPost(path string, body any) (int, map[string]any) {
	w.t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:9"
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestDirectPlacementIsLinkedToItsMarketplaceOrderInTheTimeline(t *testing.T) {
	w := start(t)
	status, placed := w.sellerPost("/pvt/orders", map[string]any{
		"marketplaceOrderId":          "DIRECT-1",
		"marketplaceServicesEndpoint": w.url + "/demo/mkt",
		"items":                       []any{map[string]any{"id": "6", "quantity": 1, "seller": "1", "price": 2499}},
	})
	if status != http.StatusOK {
		t.Fatalf("placement: %d %v", status, placed)
	}
	sellerID := placed["orderId"].(string)
	if status, got := w.sellerPost("/pvt/orders/"+sellerID+"/fulfill", map[string]any{"marketplaceOrderId": "DIRECT-1"}); status != http.StatusOK {
		t.Fatalf("fulfill: %d %v", status, got)
	}

	w.eventually("the order to ship", func() bool {
		o := w.orderState("DIRECT-1")
		return o != nil && o["status"] == "shipped" && strings.Contains(sources(o), "despacho concluído")
	})

	orders := w.state()["orders"].([]any)
	if len(orders) != 1 {
		t.Fatalf("panel orders = %d, want 1 (no duplicate entry under the seller id): %v", len(orders), orders)
	}
	if o := orders[0].(map[string]any); o["status"] == "not_created" {
		t.Fatalf("entry is not_created: %v", o)
	}
}

func TestAFailedSimulationLeavesNoOrderEntryInThePanel(t *testing.T) {
	w := start(t)
	w.tms.mu.Lock()
	w.tms.quotesFail = true
	w.tms.mu.Unlock()

	if status, _ := w.do("POST", "/demo/orders", map[string]any{"marketplaceOrderId": "SIM-FAIL", "sku": "6", "quantity": 1}); status == http.StatusOK {
		t.Fatal("placement should have failed with the quote down")
	}

	if orders := w.state()["orders"].([]any); len(orders) != 0 {
		t.Fatalf("panel orders = %v, want none", orders)
	}
}

func TestResetKeepsTheERPFlagWhenResettingTheMockFails(t *testing.T) {
	w := start(t)
	w.do("POST", "/demo/erp", map[string]any{"failing": true})
	w.erp.mu.Lock()
	w.erp.resetFails = true
	w.erp.mu.Unlock()

	if status, _ := w.do("POST", "/demo/reset", nil); status != http.StatusInternalServerError {
		t.Fatalf("reset status = %d, want 500", status)
	}

	if failing := w.state()["erp"].(map[string]any)["failing"]; failing != true {
		t.Fatalf("erp.failing = %v: the mock is still down, the panel must say so", failing)
	}
}

func TestInternalInvoiceCallbackIsNotRateLimited(t *testing.T) {
	w := start(t, func(o *options) { o.rateBurst, o.ratePerSecond = 3, 0.001 })
	w.place("RL-1")
	for i := 0; i < 5; i++ {
		w.do("GET", "/demo/state", nil)
	}
	if status, _ := w.do("GET", "/demo/state", nil); status != http.StatusTooManyRequests {
		t.Fatalf("setup: the public budget should be spent, got %d", status)
	}

	if code := w.invoiceFrom("127.0.0.1:1", "", "RL-1", map[string]any{"type": "Output", "invoiceNumber": "INV-RL", "trackingNumber": "T"}); code != http.StatusOK {
		t.Fatalf("internal callback: status = %d, want 200 even with the budget spent", code)
	}
}

func TestInvoiceValueInTheTimelineIncludesTheFreightFromTheSimulation(t *testing.T) {
	w := start(t)
	w.place("FREIGHT-1")
	w.fulfill("FREIGHT-1")

	w.eventually("invoice received", func() bool {
		o := w.orderState("FREIGHT-1")
		return o != nil && strings.Contains(sources(o), "marketplace recebeu a nota")
	})

	// 2 x R$ 24,99 of items plus the R$ 10,99 freight the TMS quoted.
	if got := sources(w.orderState("FREIGHT-1")); !strings.Contains(got, "valor R$ 60,97") {
		t.Fatalf("timeline lacks the invoice value with freight:\n%s", got)
	}
}
