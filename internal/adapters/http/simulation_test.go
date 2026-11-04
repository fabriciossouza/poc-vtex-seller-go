package httpadapter_test

import (
	"net/http"
	"testing"
)

func TestSimulationMatchesTheContractResponse(t *testing.T) {
	e := newEnv(t, map[string]int{"6": 199})
	contract := readJSONFile(t, "simulation_response.json")

	status, got := e.post(t, "/pvt/orderForms/simulation", readJSONFile(t, "simulation_request.json"))

	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, got)
	}
	assertHasFields(t, got, "country", "items", "logisticsInfo", "postalCode")
	assertWithinContract(t, "response", got, contract)
	if got["country"] != "BRA" || got["postalCode"] != "22270030" {
		t.Errorf("country/postalCode = %v / %v", got["country"], got["postalCode"])
	}
	item := got["items"].([]any)[0].(map[string]any)
	if item["id"] != "6" || item["seller"] != "01" || item["quantity"] != float64(1) || item["price"] != float64(2499) || item["requestIndex"] != float64(0) {
		t.Errorf("item = %v", item)
	}
	info := got["logisticsInfo"].([]any)[0].(map[string]any)
	sla := info["slas"].([]any)[0].(map[string]any)
	if info["itemIndex"] != float64(0) || info["stockBalance"] != float64(199) || sla["price"] != float64(1099) || sla["shippingEstimate"] != "7d" {
		t.Errorf("logisticsInfo = %v", info)
	}
}

func TestSimulationReportsUnavailableItemWithZeroStockAndNoSLA(t *testing.T) {
	e := newEnv(t, map[string]int{"6": 0})

	status, got := e.post(t, "/pvt/orderForms/simulation", readJSONFile(t, "simulation_request.json"))

	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	info := got["logisticsInfo"].([]any)[0].(map[string]any)
	if info["stockBalance"] != float64(0) || len(info["slas"].([]any)) != 0 {
		t.Fatalf("logisticsInfo = %v", info)
	}
}

func TestSimulationRejectsMalformedAndEmptyRequests(t *testing.T) {
	e := newEnv(t, nil)
	cases := map[string]string{
		"not json":    `{`,
		"no items":    `{"postalCode":"22270-030","items":[]}`,
		"zero amount": `{"items":[{"id":"6","quantity":0,"seller":"01"}]}`,
		"empty id":    `{"items":[{"id":"","quantity":1,"seller":"01"}]}`,
	}
	for name, body := range cases {
		status, got := e.post(t, "/pvt/orderForms/simulation", body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, status)
		}
		if _, ok := got["error"]; !ok {
			t.Errorf("%s: body = %v, want an error object", name, got)
		}
	}
}

func TestSimulationAcceptsStorefrontRequestWithoutAddress(t *testing.T) {
	e := newEnv(t, map[string]int{"6": 5})

	status, _ := e.post(t, "/pvt/orderForms/simulation", `{"items":[{"id":"6","quantity":1,"seller":"01"}]}`)

	if status != http.StatusOK {
		t.Fatalf("status = %d: country/postalCode may be absent in storefront simulations", status)
	}
}
