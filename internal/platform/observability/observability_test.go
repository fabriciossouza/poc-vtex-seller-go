package observability_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/observability"
)

func routed() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "x", http.StatusInternalServerError) })
	return mux
}

func scrape(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestMetricsCountRequestsByRoutePatternAndStatusNotByRawPath(t *testing.T) {
	m := observability.NewMetrics()
	h := m.Middleware(routed())

	for _, path := range []string{"/orders/1", "/orders/2", "/boom", "/nowhere/abc123"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	out := scrape(t, m)

	for _, want := range []string{
		`http_requests_total{code="201",method="GET",route="GET /orders/{id}"} 2`,
		`http_requests_total{code="500",method="GET",route="GET /boom"} 1`,
		`http_requests_total{code="404",method="GET",route="unmatched"} 1`,
		`http_request_duration_seconds_count{method="GET",route="GET /orders/{id}"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "abc123") || strings.Contains(out, "/orders/1") {
		t.Error("raw paths must not become label values")
	}
}

func TestMetricsExposeRegisteredGaugesReadAtScrapeTime(t *testing.T) {
	m := observability.NewMetrics()
	depth := 2.0
	m.Gauge("dlq_depth", "Messages in the dead-letter queue.", func() float64 { return depth })

	if out := scrape(t, m); !strings.Contains(out, "dlq_depth 2") {
		t.Fatalf("metrics = %s", out)
	}
	depth = 5
	if out := scrape(t, m); !strings.Contains(out, "dlq_depth 5") {
		t.Fatalf("gauge was not read again: %s", out)
	}
}

func TestRequestLogWritesOneStructuredJSONLinePerRequest(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := observability.RequestLog(logger, func(*http.Request) string { return "198.51.100.9" })(routed())
	req := httptest.NewRequest(http.MethodGet, "/orders/7?token=secret", nil)

	h.ServeHTTP(httptest.NewRecorder(), req)

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, buf.String())
	}
	if line["msg"] != "request" || line["method"] != "GET" || line["path"] != "/orders/7" ||
		line["status"] != float64(201) || line["client_ip"] != "198.51.100.9" {
		t.Fatalf("line = %v", line)
	}
	if _, ok := line["duration_ms"]; !ok {
		t.Fatal("duration_ms missing")
	}
	if strings.Contains(buf.String(), "secret") {
		t.Fatal("the query string must not be logged")
	}
}

func TestMetricsFoldNonStandardMethodsIntoOTHER(t *testing.T) {
	m := observability.NewMetrics()
	h := m.Middleware(routed())

	for _, method := range []string{"X-ATTACK-1", "X-ATTACK-2", "PROPFIND", "get"} {
		req := httptest.NewRequest(http.MethodGet, "/orders/1", nil)
		req.Method = method
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))
	out := scrape(t, m)

	if strings.Contains(out, "ATTACK") || strings.Contains(out, "PROPFIND") || strings.Contains(out, `method="get"`) {
		t.Errorf("arbitrary methods became label values\n%s", out)
	}
	if !strings.Contains(out, `method="OTHER"`) || !strings.Contains(out, `method="GET"`) {
		t.Errorf("want OTHER and GET labels\n%s", out)
	}
}
