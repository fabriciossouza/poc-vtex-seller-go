package tms_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/tms"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type captured struct {
	method, path, idempotencyKey string
	body                         map[string]any
}

func serve(t *testing.T, status int, response string) (*tms.Client, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.idempotencyKey = r.Method, r.URL.Path, r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return tms.New(srv.URL, nil), got
}

func TestQuoteShippingPostsItemsAndMapsTheSLA(t *testing.T) {
	client, got := serve(t, 200, `{"priceCents":1099,"estimateHours":168}`)

	sla, err := client.QuoteShipping(context.Background(), []domain.Item{{SKU: "6", Quantity: 2}})
	if err != nil {
		t.Fatalf("QuoteShipping: %v", err)
	}

	if sla.PriceCents != 1099 || sla.Estimate != 168*time.Hour {
		t.Fatalf("sla = %+v", sla)
	}
	item := got.body["items"].([]any)[0].(map[string]any)
	if got.method != "POST" || got.path != "/quotes" || item["sku"] != "6" || item["quantity"] != float64(2) {
		t.Fatalf("request = %+v", got)
	}
}

func TestCreateShipmentReturnsTheTrackingCodeAndKeysTheCallByOrder(t *testing.T) {
	client, got := serve(t, 201, `{"trackingCode":"TRK-9"}`)

	code, err := client.CreateShipment(context.Background(), domain.Order{
		ID: "959311095", IdempotencyKey: "1138342255777-01", Items: []domain.Item{{SKU: "6", Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("CreateShipment: %v", err)
	}

	if code != "TRK-9" {
		t.Fatalf("tracking = %q", code)
	}
	if got.path != "/shipments" || got.idempotencyKey != "959311095" || got.body["orderId"] != "959311095" || got.body["marketplaceOrderId"] != "1138342255777-01" {
		t.Fatalf("request = %+v", got)
	}
}

func TestCreateShipmentRejectsAnEmptyTrackingCode(t *testing.T) {
	client, _ := serve(t, 200, `{}`)
	if _, err := client.CreateShipment(context.Background(), domain.Order{ID: "o"}); err == nil {
		t.Fatal("a shipment without tracking code must be an error")
	}
}

func TestTMSReportsStatusErrorsForBothCalls(t *testing.T) {
	for status, transient := range map[int]bool{http.StatusBadGateway: true, http.StatusBadRequest: false} {
		client, _ := serve(t, status, `{}`)
		_, quoteErr := client.QuoteShipping(context.Background(), nil)
		_, shipErr := client.CreateShipment(context.Background(), domain.Order{ID: "o"})
		for _, err := range []error{quoteErr, shipErr} {
			var se *ports.StatusError
			if !errors.As(err, &se) || se.StatusCode != status || se.Transient() != transient {
				t.Errorf("status %d: err = %v", status, err)
			}
		}
	}
}
