package tms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a client for the TMS service; a nil httpClient gets a 5s timeout.
func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}
}

type item struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

func toItems(items []domain.Item) []item {
	out := make([]item, 0, len(items))
	for _, it := range items {
		out = append(out, item{SKU: it.SKU, Quantity: it.Quantity})
	}
	return out
}

func (c *Client) QuoteShipping(ctx context.Context, items []domain.Item) (domain.ShippingSLA, error) {
	var resp struct {
		PriceCents    int64 `json:"priceCents"`
		EstimateHours int64 `json:"estimateHours"`
	}
	if err := c.post(ctx, "/quotes", "", map[string]any{"items": toItems(items)}, &resp); err != nil {
		return domain.ShippingSLA{}, fmt.Errorf("quote shipping: %w", err)
	}
	return domain.ShippingSLA{PriceCents: resp.PriceCents, Estimate: time.Duration(resp.EstimateHours) * time.Hour}, nil
}

// CreateShipment keys the call by the seller order id so a retried request
// returns the shipment already created instead of booking a second one.
func (c *Client) CreateShipment(ctx context.Context, order domain.Order) (string, error) {
	var resp struct {
		TrackingCode string `json:"trackingCode"`
	}
	payload := map[string]any{"orderId": order.ID, "marketplaceOrderId": order.IdempotencyKey, "items": toItems(order.Items)}
	if err := c.post(ctx, "/shipments", order.ID, payload, &resp); err != nil {
		return "", fmt.Errorf("create shipment: %w", err)
	}
	if resp.TrackingCode == "" {
		return "", errors.New("create shipment: TMS returned no tracking code")
	}
	return resp.TrackingCode, nil
}

func (c *Client) post(ctx context.Context, path, idempotencyKey string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return &ports.StatusError{Service: "tms", StatusCode: resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
