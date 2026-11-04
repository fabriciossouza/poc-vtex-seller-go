package sapadapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/sapadapter"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

var order = domain.Order{
	ID:             "959311095",
	IdempotencyKey: "1138342255777-01",
	Items:          []domain.Item{{SKU: "6", Quantity: 2, PriceCents: 2499}},
}

type captured struct {
	method, path, idempotencyKey string
	body                         map[string]any
}

func serve(t *testing.T, status int) (*sapadapter.Client, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.idempotencyKey = r.Method, r.URL.Path, r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return sapadapter.New(srv.URL, nil), got
}

func TestSendOrderPostsTheOrderWithAnIdempotencyKey(t *testing.T) {
	client, got := serve(t, http.StatusCreated)

	if err := client.SendOrder(context.Background(), order); err != nil {
		t.Fatalf("SendOrder: %v", err)
	}

	if got.method != "POST" || got.path != "/orders" || got.idempotencyKey != "959311095" {
		t.Fatalf("request = %+v", got)
	}
	items := got.body["items"].([]any)[0].(map[string]any)
	if got.body["orderId"] != "959311095" || got.body["marketplaceOrderId"] != "1138342255777-01" ||
		items["sku"] != "6" || items["quantity"] != float64(2) || items["priceCents"] != float64(2499) {
		t.Fatalf("body = %v", got.body)
	}
}

func TestSendOrderTreatsConflictAsAlreadyDelivered(t *testing.T) {
	client, _ := serve(t, http.StatusConflict)
	if err := client.SendOrder(context.Background(), order); err != nil {
		t.Fatalf("409 means the ERP already has the order: %v", err)
	}
}

func TestSendOrderReportsStatusErrors(t *testing.T) {
	for status, transient := range map[int]bool{http.StatusInternalServerError: true, http.StatusUnprocessableEntity: false} {
		client, _ := serve(t, status)
		err := client.SendOrder(context.Background(), order)
		var se *ports.StatusError
		if !errors.As(err, &se) || se.StatusCode != status || se.Transient() != transient {
			t.Errorf("status %d: err = %v", status, err)
		}
	}
}

func TestSendOrderReportsConnectionFailures(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	err := sapadapter.New(url, nil).SendOrder(context.Background(), order)

	if err == nil {
		t.Fatal("want connection error")
	}
}
