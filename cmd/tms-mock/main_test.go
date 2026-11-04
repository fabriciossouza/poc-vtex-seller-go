package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func call(t *testing.T, h http.Handler, method, path, body, key string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestQuoteAnswersPriceAndEstimateForTheItems(t *testing.T) {
	status, got := call(t, newHandler(), "POST", "/quotes", `{"items":[{"sku":"6","quantity":2}]}`, "")

	if status != 200 || got["priceCents"] != float64(1099) || got["estimateHours"] != float64(168) {
		t.Fatalf("status = %d, body = %v", status, got)
	}
}

func TestQuoteRejectsEmptyOrBrokenBodies(t *testing.T) {
	for _, body := range []string{`{"items":[]}`, `{nope`} {
		if status, _ := call(t, newHandler(), "POST", "/quotes", body, ""); status != 400 {
			t.Errorf("body %q: status = %d, want 400", body, status)
		}
	}
}

func TestShipmentIsIdempotentByKeyAndCountsEachKeyOnce(t *testing.T) {
	h := newHandler()
	body := `{"orderId":"o1","marketplaceOrderId":"m1","items":[{"sku":"6","quantity":1}]}`

	s1, first := call(t, h, "POST", "/shipments", body, "o1")
	_, again := call(t, h, "POST", "/shipments", body, "o1")
	_, other := call(t, h, "POST", "/shipments", body, "o2")

	if s1 != 201 || first["trackingCode"] == "" || !strings.HasPrefix(first["trackingCode"].(string), "TRK-") {
		t.Fatalf("first = %d %v", s1, first)
	}
	if again["trackingCode"] != first["trackingCode"] || other["trackingCode"] == first["trackingCode"] {
		t.Fatalf("tracking codes: first %v, again %v, other %v", first, again, other)
	}
	if _, n := call(t, h, "GET", "/admin/shipments", "", ""); n["count"] != float64(2) {
		t.Fatalf("count = %v, want 2", n)
	}
}

func TestShipmentRequiresAnIdempotencyKey(t *testing.T) {
	if status, _ := call(t, newHandler(), "POST", "/shipments", `{"items":[{"sku":"6","quantity":1}]}`, ""); status != 400 {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestResetForgetsShipments(t *testing.T) {
	h := newHandler()
	call(t, h, "POST", "/shipments", `{"items":[{"sku":"6","quantity":1}]}`, "o1")

	call(t, h, "POST", "/admin/reset", "", "")

	if _, n := call(t, h, "GET", "/admin/shipments", "", ""); n["count"] != float64(0) {
		t.Fatalf("count = %v, want 0", n)
	}
}
