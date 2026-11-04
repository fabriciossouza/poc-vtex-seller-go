package marketplace_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/marketplace"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

var issuedAt = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

type recorder struct {
	mu     sync.Mutex
	paths  []string
	query  []string
	bodies []map[string]any
	status []int
}

func (r *recorder) handler(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(req.Body).Decode(&body)
	r.query = append(r.query, req.URL.RawQuery)
	r.paths = append(r.paths, req.Method+" "+req.URL.Path+" "+req.Header.Get("Content-Type"))
	r.bodies = append(r.bodies, body)
	code := http.StatusOK
	if len(r.status) > 0 {
		code, r.status = r.status[0], r.status[1:]
	}
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"receipt":"x"}`))
}

func setup(t *testing.T, statuses ...int) (*marketplace.Client, *recorder, string) {
	t.Helper()
	rec := &recorder{status: statuses}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	client := marketplace.New(marketplace.Config{
		AllowedHosts: []string{u.Host},
		Now:          func() time.Time { return issuedAt },
	})
	return client, rec, srv.URL + "/"
}

func shippedOrder(endpoint string) domain.Order {
	return domain.Order{
		ID:                  "959311095",
		IdempotencyKey:      "1138342255777-01",
		Items:               []domain.Item{{SKU: "6", Quantity: 2, PriceCents: 2499}},
		Status:              domain.StatusShipped,
		TrackingCode:        "12345678abc",
		MarketplaceEndpoint: endpoint,
	}
}

func contractKeys(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../../testdata/invoice_request.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNotifyInvoicePostsContractBodyToTheOrderEndpoint(t *testing.T) {
	client, rec, endpoint := setup(t)

	if err := client.NotifyInvoice(context.Background(), shippedOrder(endpoint)); err != nil {
		t.Fatalf("NotifyInvoice: %v", err)
	}

	if want := "POST /pvt/orders/1138342255777-01/invoice application/json"; len(rec.paths) != 1 || rec.paths[0] != want {
		t.Fatalf("requests = %v, want %q", rec.paths, want)
	}
	body, contract := rec.bodies[0], contractKeys(t)
	for key := range body {
		if _, ok := contract[key]; !ok {
			t.Errorf("body field %q is not in the contract payload", key)
		}
	}
	if body["type"] != "Output" || body["invoiceNumber"] != "INV-959311095" || body["trackingNumber"] != "12345678abc" {
		t.Errorf("body = %v", body)
	}
	if body["invoiceValue"] != float64(4998) || body["issuanceDate"] != "2026-11-05T10:00:00" {
		t.Errorf("value/date = %v / %v", body["invoiceValue"], body["issuanceDate"])
	}
	items := body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != "6" || items[0].(map[string]any)["quantity"] != float64(2) || items[0].(map[string]any)["price"] != float64(2499) {
		t.Errorf("items = %v", items)
	}
}

func TestNotifyInvoiceToleratesARepeatedNotification(t *testing.T) {
	client, rec, endpoint := setup(t, http.StatusOK, http.StatusConflict)
	order := shippedOrder(endpoint)

	if err := client.NotifyInvoice(context.Background(), order); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := client.NotifyInvoice(context.Background(), order); err != nil {
		t.Fatalf("repeated (409) must be tolerated: %v", err)
	}
	if rec.bodies[0]["invoiceNumber"] != rec.bodies[1]["invoiceNumber"] {
		t.Fatal("repeated notification must carry the same invoice number")
	}
}

func TestNotifyInvoiceClassifiesFailuresForRetry(t *testing.T) {
	for status, wantTransient := range map[int]bool{http.StatusServiceUnavailable: true, http.StatusBadRequest: false} {
		client, _, endpoint := setup(t, status)

		err := client.NotifyInvoice(context.Background(), shippedOrder(endpoint))

		var statusErr *ports.StatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode != status {
			t.Fatalf("status %d: err = %v", status, err)
		}
		if statusErr.Transient() != wantTransient {
			t.Errorf("status %d: transient = %v", status, statusErr.Transient())
		}
	}
}

func TestNotifyInvoiceRefusesEndpointsOutsideTheAllowlist(t *testing.T) {
	client, rec, _ := setup(t)
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request reached a host that is not allowed")
	}))
	defer other.Close()

	for name, endpoint := range map[string]string{"other host": other.URL, "empty": "", "not http": "file:///etc/passwd"} {
		err := client.NotifyInvoice(context.Background(), shippedOrder(endpoint))
		if !errors.Is(err, marketplace.ErrEndpointNotAllowed) {
			t.Errorf("%s: err = %v, want ErrEndpointNotAllowed", name, err)
		}
	}
	if len(rec.paths) != 0 {
		t.Fatalf("requests = %v", rec.paths)
	}
}

func TestNotifyInvoiceKeepsTheEndpointPathPrefixAndQueryString(t *testing.T) {
	client, rec, endpoint := setup(t)

	if err := client.NotifyInvoice(context.Background(), shippedOrder(endpoint+"api/?an=seller&sc=1")); err != nil {
		t.Fatalf("NotifyInvoice: %v", err)
	}

	if want := "POST /api/pvt/orders/1138342255777-01/invoice application/json"; len(rec.paths) != 1 || rec.paths[0] != want {
		t.Fatalf("requests = %v, want %q", rec.paths, want)
	}
	if rec.query[0] != "an=seller&sc=1" {
		t.Fatalf("query = %q", rec.query[0])
	}
}

func TestValidateEndpointAcceptsOnlyAllowedHostsAndNeverEchoesTheValue(t *testing.T) {
	client, _, endpoint := setup(t)
	if err := client.ValidateEndpoint(endpoint); err != nil {
		t.Fatalf("allowed endpoint: %v", err)
	}

	secret := "http://internal.example:9/path?token=s3cret"
	err := client.ValidateEndpoint(secret)

	if !errors.Is(err, marketplace.ErrEndpointNotAllowed) {
		t.Fatalf("err = %v", err)
	}
	for _, leaked := range []string{"internal.example", "s3cret"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("error message %q leaks %q (it ends up in the DLQ reason)", err, leaked)
		}
	}
	notifyErr := client.NotifyInvoice(context.Background(), shippedOrder(secret))
	if notifyErr == nil || strings.Contains(notifyErr.Error(), "s3cret") {
		t.Errorf("NotifyInvoice error = %v", notifyErr)
	}
}

func TestNotifyInvoiceAddsTheFreightToTheInvoiceValueAsInTheContractExample(t *testing.T) {
	client, rec, endpoint := setup(t)
	order := shippedOrder(endpoint)
	order.Items[0].Quantity = 1
	order.FreightCents = 1099

	if err := client.NotifyInvoice(context.Background(), order); err != nil {
		t.Fatalf("NotifyInvoice: %v", err)
	}

	body, contract := rec.bodies[0], contractKeys(t)
	if body["invoiceValue"] != contract["invoiceValue"] {
		t.Errorf("invoiceValue = %v, contract example = %v", body["invoiceValue"], contract["invoiceValue"])
	}
	if got, want := body["items"], contract["items"]; !reflect.DeepEqual(got, want) {
		t.Errorf("items = %v, contract example = %v", got, want)
	}
}
