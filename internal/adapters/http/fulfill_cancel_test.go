package httpadapter_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

func placeContractOrder(t *testing.T, e *env) string {
	t.Helper()
	status, got := e.post(t, "/pvt/orders", readJSONFile(t, "order_placement_request.json"))
	if status != http.StatusOK {
		t.Fatalf("placement status = %d", status)
	}
	return got["orderId"].(string)
}

func assertReceipt(t *testing.T, got map[string]any, contractFile, orderID string) {
	t.Helper()
	assertHasFields(t, got, "date", "marketplaceOrderId", "orderId", "receipt")
	assertWithinContract(t, "response", got, readJSONFile(t, contractFile))
	if got["date"] != "2026-11-04 18:52:00" || got["marketplaceOrderId"] != "1138342255777-01" || got["orderId"] != orderID || got["receipt"] == "" {
		t.Errorf("response = %v", got)
	}
}

func TestFulfillAuthorizesTheOrderAndAnswersWithAReceipt(t *testing.T) {
	e := newEnv(t, nil)
	orderID := placeContractOrder(t, e)

	status, got := e.post(t, "/pvt/orders/"+orderID+"/fulfill", readJSONFile(t, "fulfill_request.json"))

	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, got)
	}
	assertReceipt(t, got, "fulfill_response.json", orderID)
	stored, _ := e.repo.Get(context.Background(), orderID)
	if stored.Status != domain.StatusAuthorized {
		t.Errorf("status = %s", stored.Status)
	}
	if len(e.bus.events) != 1 || e.bus.events[0] != (domain.Event{Type: domain.EventOrderAuthorized, OrderID: orderID}) {
		t.Errorf("events = %+v", e.bus.events)
	}
}

func TestFulfillRequiresOnlyTheMarketplaceOrderID(t *testing.T) {
	e := newEnv(t, nil)
	orderID := placeContractOrder(t, e)

	if status, got := e.post(t, "/pvt/orders/"+orderID+"/fulfill", `{"marketplaceOrderId":"1138342255777-01"}`); status != http.StatusOK {
		t.Fatalf("minimal body: status = %d, body = %v", status, got)
	}
}

func TestFulfillRejectsABodyWithoutMarketplaceOrderID(t *testing.T) {
	e := newEnv(t, nil)
	orderID := placeContractOrder(t, e)

	for _, body := range []string{`{}`, `{`} {
		if status, _ := e.post(t, "/pvt/orders/"+orderID+"/fulfill", body); status != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, status)
		}
	}
	if len(e.bus.events) != 0 {
		t.Fatalf("events = %+v", e.bus.events)
	}
}

func TestFulfillMapsDomainErrorsToStatus(t *testing.T) {
	e := newEnv(t, nil)
	orderID := placeContractOrder(t, e)
	body := `{"marketplaceOrderId":"1138342255777-01"}`
	_, _ = e.post(t, "/pvt/orders/"+orderID+"/fulfill", body)

	if status, _ := e.post(t, "/pvt/orders/nope/fulfill", body); status != http.StatusNotFound {
		t.Errorf("unknown order: status = %d, want 404", status)
	}
	if status, _ := e.post(t, "/pvt/orders/"+orderID+"/fulfill", body); status != http.StatusConflict {
		t.Errorf("second fulfill: status = %d, want 409", status)
	}
	if len(e.bus.events) != 1 {
		t.Errorf("events = %d, want 1", len(e.bus.events))
	}
}

func TestCancelConfirmsWithAReceiptAndCancelsTheOrder(t *testing.T) {
	e := newEnv(t, nil)
	orderID := placeContractOrder(t, e)

	status, got := e.post(t, "/pvt/orders/"+orderID+"/cancel", readJSONFile(t, "cancel_request.json"))

	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, got)
	}
	assertReceipt(t, got, "cancel_response.json", orderID)
	stored, _ := e.repo.Get(context.Background(), orderID)
	if stored.Status != domain.StatusCancelled {
		t.Errorf("status = %s", stored.Status)
	}
}

func TestCancelRequiresTheContractFields(t *testing.T) {
	e := newEnv(t, nil)
	orderID := placeContractOrder(t, e)
	for _, field := range []string{"marketplaceOrderId", "marketplaceOrderGroup", "cancellationRequestId", "cancellationRequestDate", "reason", "requestedByUser"} {
		body := readJSONFile(t, "cancel_request.json")
		delete(body, field)
		if status, _ := e.post(t, "/pvt/orders/"+orderID+"/cancel", body); status != http.StatusBadRequest {
			t.Errorf("without %s: status = %d, want 400", field, status)
		}
	}
	if stored, _ := e.repo.Get(context.Background(), orderID); stored.Status != domain.StatusReceived {
		t.Fatalf("status = %s: rejected requests must not cancel", stored.Status)
	}
}

func TestCancelMapsDomainErrorsToStatus(t *testing.T) {
	e := newEnv(t, nil)
	orderID := placeContractOrder(t, e)
	shipped, _ := e.repo.Get(context.Background(), orderID)
	_ = shipped.Authorize()
	_ = shipped.MarkSentToERP()
	_ = shipped.MarkShipped("TRK")
	_ = e.repo.Save(context.Background(), shipped)
	body := readJSONFile(t, "cancel_request.json")

	if status, _ := e.post(t, "/pvt/orders/nope/cancel", body); status != http.StatusNotFound {
		t.Errorf("unknown order: status = %d, want 404", status)
	}
	if status, _ := e.post(t, "/pvt/orders/"+orderID+"/cancel", body); status != http.StatusConflict {
		t.Errorf("shipped order: status = %d, want 409 (the marketplace must refuse by sending the invoice)", status)
	}
}
