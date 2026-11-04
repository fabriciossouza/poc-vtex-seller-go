package sapadapter

import (
	"bytes"
	"context"
	"encoding/json"
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

// New returns a client for the ERP adapter service; a nil httpClient gets a 5s timeout.
func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}
}

type orderItem struct {
	SKU        string `json:"sku"`
	Quantity   int    `json:"quantity"`
	PriceCents int64  `json:"priceCents"`
}

type orderRequest struct {
	OrderID            string      `json:"orderId"`
	MarketplaceOrderID string      `json:"marketplaceOrderId"`
	Items              []orderItem `json:"items"`
}

// SendOrder keys the call by the seller order id and treats 409 as success,
// so a retry after a lost response does not create a second ERP order.
func (c *Client) SendOrder(ctx context.Context, order domain.Order) error {
	payload := orderRequest{OrderID: order.ID, MarketplaceOrderID: order.IdempotencyKey}
	for _, it := range order.Items {
		payload.Items = append(payload.Items, orderItem{SKU: it.SKU, Quantity: it.Quantity, PriceCents: it.PriceCents})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode order: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/orders", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build ERP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", order.ID)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("send order to ERP: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusConflict {
		return nil
	}
	return &ports.StatusError{Service: "erp", StatusCode: resp.StatusCode}
}
