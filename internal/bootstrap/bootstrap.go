// Package bootstrap wires the adapters, use cases, worker and demo into one
// HTTP handler. main only reads the environment and runs what it returns.
package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"time"

	"github.com/redis/go-redis/v9"

	httpadapter "github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/http"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/inventory"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/marketplace"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/queue"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/sapadapter"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/sqlite"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/tms"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/demo"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/observability"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/ratelimit"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/resilience"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/worker"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/web"
)

type Config struct {
	Redis  redis.Cmdable
	DBPath string
	ERPURL string
	TMSURL string
	// MarketplaceAllowedHosts is the host[:port] allowlist for the invoice
	// callback; the demo simulator lives at MarketplaceEndpoint.
	MarketplaceAllowedHosts []string
	MarketplaceEndpoint     string
	// DependencyTimeout bounds each call to the ERP, TMS and marketplace.
	DependencyTimeout  time.Duration
	Retry              resilience.RetryConfig
	Breaker            resilience.BreakerConfig
	QueueBlock         time.Duration
	RateLimitPerSecond float64
	RateLimitBurst     int
	TrustedProxies     []netip.Prefix
}

type App struct {
	Handler http.Handler
	Worker  *worker.Worker
	// Reset clears the demo state; main also calls it every hour.
	Reset func(ctx context.Context) error
	Close func()
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func New(ctx context.Context, cfg Config) (*App, error) {
	repo, err := sqlite.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	catalog, stock := inventory.DefaultCatalog(), inventory.DefaultStock()
	timeline := demo.NewTimeline(time.Now, 200, 60)
	httpClient := &http.Client{Timeout: cfg.DependencyTimeout}

	// One dispatch attempt makes up to three calls (ERP, TMS, marketplace), so
	// the idle time before redelivery covers that and stays above the
	// documented MaxAttempts x (timeout + MaxDelay).
	claimIdle := time.Duration(cfg.Retry.MaxAttempts) * (3*cfg.DependencyTimeout + cfg.Retry.MaxDelay)
	rawBus, err := queue.New(ctx, cfg.Redis, queue.Config{
		Stream: "orders.events", Group: "seller-workers", DLQ: "orders.dlq", Consumer: "seller-1",
		Block: cfg.QueueBlock, ClaimIdle: claimIdle,
	})
	if err != nil {
		_ = repo.Close()
		return nil, err
	}

	erp := demo.NewRecordingERP(sapadapter.New(cfg.ERPURL, httpClient), timeline)
	shipping := demo.NewRecordingTMS(tms.New(cfg.TMSURL, httpClient), timeline)
	marketplaceClient := marketplace.New(marketplace.Config{AllowedHosts: cfg.MarketplaceAllowedHosts, HTTPClient: httpClient})
	notifier := demo.NewRecordingMarketplace(marketplaceClient, timeline)
	bus := demo.NewRecordingBus(rawBus, timeline)
	consumer := demo.NewRecordingConsumer(rawBus, timeline)

	seller := httpadapter.NewHandler(httpadapter.Deps{
		Simulate:         app.NewSimulateCart(catalog, stock, shipping),
		Place:            app.NewPlaceOrder(repo, systemClock{}, stock),
		Fulfill:          app.NewFulfillOrder(repo, bus),
		Cancel:           app.NewCancelOrder(repo),
		ValidateEndpoint: marketplaceClient.ValidateEndpoint,
		Clock:            systemClock{},
		NewID:            newID,
	})

	breaker := resilience.NewBreaker(cfg.Breaker, time.Now)
	retrier := resilience.NewRetrier(cfg.Retry, resilience.SystemSleeper{}, nil)
	w := worker.New(consumer, app.NewDispatchToERP(repo, erp, shipping, notifier), retrier, breaker)

	controller := demo.NewController(demo.Deps{
		Seller:             seller,
		Endpoint:           cfg.MarketplaceEndpoint,
		Orders:             repo,
		Timeline:           timeline,
		Breaker:            breaker,
		BreakerOpenTimeout: cfg.Breaker.OpenTimeout,
		DeadLetters:        consumer.DeadLetters,
		Replay:             w.ReplayDLQ,
		Stock:              func() []demo.StockLine { return stockLines(catalog, stock) },
		SetERPFailing: func(ctx context.Context, failing bool) error {
			rate := 0.0
			if failing {
				rate = 1
			}
			return sendJSON(ctx, httpClient, http.MethodPut, cfg.ERPURL+"/admin/config", map[string]any{"errorRate": rate, "latencyMs": 0})
		},
		Resets: []func(context.Context) error{
			repo.Reset,
			rawBus.Reset,
			func(context.Context) error { breaker.Reset(); return nil },
			func(context.Context) error { stock.Reset(); return nil },
			func(ctx context.Context) error {
				return sendJSON(ctx, httpClient, http.MethodPost, cfg.ERPURL+"/admin/reset", nil)
			},
			func(ctx context.Context) error {
				return sendJSON(ctx, httpClient, http.MethodPost, cfg.TMSURL+"/admin/reset", nil)
			},
		},
		NewID: newID,
		Now:   time.Now,
	})

	metrics := observability.NewMetrics()
	metrics.Gauge("breaker_open", "1 while the dispatch circuit breaker is open.", func() float64 {
		if breaker.Snapshot().State == resilience.StateOpen {
			return 1
		}
		return 0
	})
	metrics.Gauge("breaker_consecutive_failures", "Consecutive transient failures seen by the breaker.", func() float64 {
		return float64(breaker.Snapshot().ConsecutiveFailures)
	})
	metrics.Gauge("dlq_depth", "Messages waiting in the dead-letter queue.", func() float64 {
		letters, err := consumer.DeadLetters(context.Background())
		if err != nil {
			return math.NaN()
		}
		return float64(len(letters))
	})

	root := http.NewServeMux()
	root.Handle("/pvt/", seller)
	root.Handle("/demo/", controller.Handler())
	root.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) {
		if err := cfg.Redis.Ping(r.Context()).Err(); err != nil {
			http.Error(rw, `{"status":"redis unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rw, `{"status":"ok"}`+"\n")
	})
	root.Handle("GET /metrics", metrics.Handler())
	root.Handle("/", web.Handler())

	limiter := ratelimit.New(cfg.RateLimitPerSecond, cfg.RateLimitBurst, time.Now)
	var handler http.Handler = root
	handler = ratelimit.Middleware(limiter, cfg.TrustedProxies, "/healthz")(handler)
	handler = metrics.Middleware(handler)
	handler = observability.RequestLog(slog.Default(), func(r *http.Request) string { return ratelimit.ClientIP(r, cfg.TrustedProxies) })(handler)
	handler = securityHeaders(handler)

	return &App{
		Handler: handler,
		Worker:  w,
		Reset:   controller.Reset,
		Close:   func() { _ = repo.Close() },
	}, nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func stockLines(catalog *inventory.Catalog, stock *inventory.Stock) []demo.StockLine {
	levels := stock.Levels()
	var lines []demo.StockLine
	for _, p := range catalog.Products() {
		lines = append(lines, demo.StockLine{SKU: p.SKU, Name: p.Name, PriceCents: p.PriceCents, Available: levels[p.SKU]})
	}
	return lines
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sendJSON calls an admin endpoint of a mock and treats any non-2xx as failure.
func sendJSON(ctx context.Context, client *http.Client, method, url string, body any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: status %d", method, url, resp.StatusCode)
	}
	return nil
}
