package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	httpadapter "github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/http"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
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

// slowTMS answers after d unless the request context ends first, like an HTTP
// client calling a slow service.
type slowTMS struct {
	fakeTMS
	d time.Duration
}

func (s slowTMS) QuoteShipping(ctx context.Context, items []domain.Item) (domain.ShippingSLA, error) {
	select {
	case <-time.After(s.d):
		return s.fakeTMS.QuoteShipping(ctx, items)
	case <-ctx.Done():
		return domain.ShippingSLA{}, ctx.Err()
	}
}

func TestSimulationAnswersBeforeTheMarketplaceTimeoutWhenTheTMSIsSlow(t *testing.T) {
	const marketplaceTimeout = 2500 * time.Millisecond
	handler := httpadapter.NewHandler(httpadapter.Deps{
		Simulate: app.NewSimulateCart(fakeCatalog{}, fakeStock{map[string]int{"6": 5}}, slowTMS{d: 2700 * time.Millisecond}),
		Clock:    fixedClock{},
		NewID:    func() string { return "x" },
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	e := &env{srv: srv}

	start := time.Now()
	status, _ := e.post(t, "/pvt/orderForms/simulation", readJSONFile(t, "simulation_request.json"))
	elapsed := time.Since(start)

	if elapsed >= marketplaceTimeout {
		t.Errorf("answered after %v, the marketplace gives up at %v", elapsed, marketplaceTimeout)
	}
	if status == http.StatusOK {
		t.Errorf("status = %d, a simulation that missed its budget must not look successful", status)
	}
}
