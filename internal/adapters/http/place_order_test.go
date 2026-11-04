package httpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	httpadapter "github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/http"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

func TestPlacementMatchesTheContractAndStoresTheOrder(t *testing.T) {
	e := newEnv(t, nil)
	contract := readJSONFile(t, "order_placement_response.json")

	status, got := e.post(t, "/pvt/orders", readJSONFile(t, "order_placement_request.json"))

	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, got)
	}
	assertHasFields(t, got, "marketplaceOrderId", "orderId", "items")
	assertWithinContract(t, "response", got, contract)
	if got["marketplaceOrderId"] != "1138342255777-01" || got["orderId"] != "959311091" {
		t.Errorf("ids = %v / %v", got["marketplaceOrderId"], got["orderId"])
	}
	item := got["items"].([]any)[0].(map[string]any)
	if item["id"] != "6" || item["quantity"] != float64(1) || item["price"] != float64(2499) {
		t.Errorf("item = %v", item)
	}
	stored, err := e.repo.Get(context.Background(), "959311091")
	if err != nil {
		t.Fatalf("order not stored: %v", err)
	}
	if stored.IdempotencyKey != "1138342255777-01" || stored.MarketplaceEndpoint != "https://marketplace-exemplo.myvtex.com/" ||
		stored.Status != domain.StatusReceived || stored.Items[0] != (domain.Item{SKU: "6", Quantity: 1, PriceCents: 2499}) {
		t.Errorf("stored = %+v", stored)
	}
}

func TestPlacementIsIdempotentByMarketplaceOrderID(t *testing.T) {
	e := newEnv(t, nil)
	body := readJSONFile(t, "order_placement_request.json")

	_, first := e.post(t, "/pvt/orders", body)
	status, second := e.post(t, "/pvt/orders", body)

	if status != http.StatusOK || second["orderId"] != first["orderId"] {
		t.Fatalf("status = %d, second orderId = %v, first = %v", status, second["orderId"], first["orderId"])
	}
	if len(e.repo.byID) != 1 {
		t.Fatalf("orders stored = %d, want 1", len(e.repo.byID))
	}
}

func TestPlacementRejectsInvalidRequests(t *testing.T) {
	e := newEnv(t, nil)
	cases := map[string]string{
		"not json":                `{`,
		"no marketplace order id": `{"items":[{"id":"6","quantity":1,"seller":"01","price":10}]}`,
		"no items":                `{"marketplaceOrderId":"m-1","items":[]}`,
		"zero quantity":           `{"marketplaceOrderId":"m-1","items":[{"id":"6","quantity":0,"seller":"01","price":10}]}`,
	}
	for name, body := range cases {
		if status, _ := e.post(t, "/pvt/orders", body); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, status)
		}
	}
	if len(e.repo.byID) != 0 {
		t.Fatalf("invalid requests created %d orders", len(e.repo.byID))
	}
}

func TestPlacementRejectsOversizedBodies(t *testing.T) {
	e := newEnv(t, nil)
	huge := `{"marketplaceOrderId":"m-1","openTextField":"` + strings.Repeat("x", 2<<20) + `"}`

	if status, _ := e.post(t, "/pvt/orders", huge); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", status)
	}
}

func TestOnlyPostIsAllowedOnPlacement(t *testing.T) {
	e := newEnv(t, nil)
	resp, err := http.Get(e.srv.URL + "/pvt/orders")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestPlacementRejectsAnEndpointOutsideTheAllowlistBeforeStoringAnything(t *testing.T) {
	e := newEnv(t, nil)
	for name, endpoint := range map[string]string{"other host": "http://169.254.169.254/", "empty": ""} {
		body := readJSONFile(t, "order_placement_request.json")
		body["marketplaceServicesEndpoint"] = endpoint

		status, got := e.post(t, "/pvt/orders", body)

		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, status)
		}
		if msg, _ := json.Marshal(got); strings.Contains(string(msg), "169.254") {
			t.Errorf("%s: response echoes the endpoint: %s", name, msg)
		}
	}
	if len(e.repo.byID) != 0 {
		t.Fatalf("orders stored = %d", len(e.repo.byID))
	}
}

func TestPlacementWithoutAnEndpointValidatorRefusesEverything(t *testing.T) {
	srv := httptest.NewServer(httpadapter.NewHandler(httpadapter.Deps{NewID: func() string { return "x" }}))
	defer srv.Close()
	e := &env{srv: srv}

	if status, _ := e.post(t, "/pvt/orders", readJSONFile(t, "order_placement_request.json")); status != http.StatusBadRequest {
		t.Fatalf("status = %d: a missing validator must fail closed", status)
	}
}

func TestPlacementRejectsAnArrayBody(t *testing.T) {
	e := newEnv(t, nil)
	raw, _ := json.Marshal([]any{readJSONFile(t, "order_placement_request.json")})

	if status, _ := e.post(t, "/pvt/orders", string(raw)); status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: the protocol sends one object, not an array", status)
	}
}

func TestPlacementAndSimulationLimitTheNumberOfItems(t *testing.T) {
	e := newEnv(t, nil)
	item := func(n int) []map[string]any {
		items := make([]map[string]any, n)
		for i := range items {
			items[i] = map[string]any{"id": "6", "quantity": 1, "seller": "01", "price": 10}
		}
		return items
	}
	placement := func(n int) map[string]any {
		return map[string]any{"marketplaceOrderId": "m-" + strconv.Itoa(n), "marketplaceServicesEndpoint": "https://marketplace-exemplo.myvtex.com/", "items": item(n)}
	}

	if status, _ := e.post(t, "/pvt/orders", placement(50)); status != http.StatusOK {
		t.Errorf("placement with 50 items: status = %d", status)
	}
	if status, _ := e.post(t, "/pvt/orders", placement(51)); status != http.StatusBadRequest {
		t.Errorf("placement with 51 items: status = %d, want 400", status)
	}
	if status, _ := e.post(t, "/pvt/orderForms/simulation", map[string]any{"items": item(50)}); status != http.StatusOK {
		t.Errorf("simulation with 50 items: status = %d", status)
	}
	if status, _ := e.post(t, "/pvt/orderForms/simulation", map[string]any{"items": item(51)}); status != http.StatusBadRequest {
		t.Errorf("simulation with 51 items: status = %d, want 400", status)
	}
}
