package httpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	httpadapter "github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/http"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
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

func TestFulfillMapsErrorsToStatusAndRepeatedFulfillSucceeds(t *testing.T) {
	e := newEnv(t, nil)
	orderID := placeContractOrder(t, e)
	body := `{"marketplaceOrderId":"1138342255777-01"}`
	_, _ = e.post(t, "/pvt/orders/"+orderID+"/fulfill", body)

	if status, _ := e.post(t, "/pvt/orders/nope/fulfill", body); status != http.StatusNotFound {
		t.Errorf("unknown order: status = %d, want 404", status)
	}
	if status, _ := e.post(t, "/pvt/orders/"+orderID+"/fulfill", body); status != http.StatusOK {
		t.Errorf("repeated fulfill: status = %d, want 200", status)
	}
	if len(e.bus.events) != 2 {
		t.Errorf("events = %d, want 2", len(e.bus.events))
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
	failed, _ := e.repo.Get(context.Background(), orderID)
	_ = failed.Fail()
	_ = e.repo.Save(context.Background(), failed)
	body := readJSONFile(t, "cancel_request.json")

	if status, _ := e.post(t, "/pvt/orders/nope/cancel", body); status != http.StatusNotFound {
		t.Errorf("unknown order: status = %d, want 404", status)
	}
	if status, _ := e.post(t, "/pvt/orders/"+orderID+"/cancel", body); status != http.StatusConflict {
		t.Errorf("failed order: status = %d, want 409", status)
	}
}

func TestCancelOfAnOrderAtTheERPOrShippedIsEvaluatedWithAnEmptyResponse(t *testing.T) {
	for _, shipped := range []bool{false, true} {
		e := newEnv(t, nil)
		orderID := placeContractOrder(t, e)
		o, _ := e.repo.Get(context.Background(), orderID)
		_ = o.Authorize()
		_ = o.MarkSentToERP()
		if shipped {
			_ = o.MarkShipped("TRK")
		}
		_ = e.repo.Save(context.Background(), o)
		raw, _ := json.Marshal(readJSONFile(t, "cancel_request.json"))

		resp, err := http.Post(e.srv.URL+"/pvt/orders/"+orderID+"/cancel", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK || len(body) != 0 {
			t.Errorf("shipped=%v: status = %d, body = %q, want 200 and an empty body (evaluating)", shipped, resp.StatusCode, body)
		}
		if stored, _ := e.repo.Get(context.Background(), orderID); stored.Status != o.Status {
			t.Errorf("shipped=%v: status = %s, the order must not change", shipped, stored.Status)
		}
	}
}

type conflictingAction struct{}

func (conflictingAction) Execute(context.Context, string, string) error {
	return ports.ErrVersionConflict
}

func TestConcurrentModificationIsReportedAsConflictSoTheMarketplaceRetries(t *testing.T) {
	handler := httpadapter.NewHandler(httpadapter.Deps{Fulfill: conflictingAction{}, Cancel: conflictingAction{}, Clock: fixedClock{}, NewID: func() string { return "x" }})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	e := &env{srv: srv}

	if status, _ := e.post(t, "/pvt/orders/o/cancel", readJSONFile(t, "cancel_request.json")); status != http.StatusConflict {
		t.Errorf("cancel: status = %d, want 409", status)
	}
	if status, _ := e.post(t, "/pvt/orders/o/fulfill", `{"marketplaceOrderId":"m"}`); status != http.StatusConflict {
		t.Errorf("fulfill: status = %d, want 409", status)
	}
}

func TestFulfillAndCancelRefuseAMarketplaceOrderIDThatIsNotTheOrders(t *testing.T) {
	for _, action := range []string{"fulfill", "cancel"} {
		e := newEnv(t, nil)
		orderID := placeContractOrder(t, e)
		body := readJSONFile(t, "cancel_request.json")
		body["marketplaceOrderId"] = "9999999999999-01"

		status, got := e.post(t, "/pvt/orders/"+orderID+"/"+action, body)

		if status != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404, body = %v", action, status, got)
		}
		if stored, _ := e.repo.Get(context.Background(), orderID); stored.Status != domain.StatusReceived {
			t.Errorf("%s: status = %s, the action must not run", action, stored.Status)
		}
		if len(e.bus.events) != 0 {
			t.Errorf("%s: events = %+v", action, e.bus.events)
		}
	}
}

func TestRepeatedCancelAnswersWithAReceiptLikeFulfill(t *testing.T) {
	e := newEnv(t, nil)
	orderID := placeContractOrder(t, e)
	body := readJSONFile(t, "cancel_request.json")
	_, _ = e.post(t, "/pvt/orders/"+orderID+"/cancel", body)

	status, got := e.post(t, "/pvt/orders/"+orderID+"/cancel", body)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %v", status, got)
	}
	assertReceipt(t, got, "cancel_response.json", orderID)
}
