package marketplace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

// ErrEndpointNotAllowed guards against SSRF: the endpoint comes from the
// placement request body, so the caller picks where the seller will connect.
var ErrEndpointNotAllowed = errors.New("marketplace endpoint not allowed")

type Config struct {
	// AllowedHosts lists the host[:port] values the client may call. Empty means none.
	AllowedHosts []string
	HTTPClient   *http.Client
	Now          func() time.Time
}

type Client struct {
	allowed map[string]bool
	http    *http.Client
	now     func() time.Time
}

func New(cfg Config) *Client {
	allowed := make(map[string]bool, len(cfg.AllowedHosts))
	for _, h := range cfg.AllowedHosts {
		allowed[strings.ToLower(h)] = true
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	// A redirect could send the request to a host outside the allowlist.
	redirectless := *httpClient
	redirectless.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Client{allowed: allowed, http: &redirectless, now: now}
}

type invoiceItem struct {
	ID       string `json:"id"`
	Quantity int    `json:"quantity"`
	Price    int64  `json:"price"`
}

type invoiceRequest struct {
	Type           string        `json:"type"`
	InvoiceNumber  string        `json:"invoiceNumber"`
	TrackingNumber string        `json:"trackingNumber"`
	Items          []invoiceItem `json:"items"`
	IssuanceDate   string        `json:"issuanceDate"`
	InvoiceValue   int64         `json:"invoiceValue"`
}

// NotifyInvoice derives the invoice number from the seller order id, so a
// repeated notification is the same invoice and a 409 means it already exists.
func (c *Client) NotifyInvoice(ctx context.Context, order domain.Order) error {
	target, err := c.invoiceURL(order)
	if err != nil {
		return err
	}
	payload := invoiceRequest{
		Type:           "Output",
		InvoiceNumber:  "INV-" + order.ID,
		TrackingNumber: order.TrackingCode,
		IssuanceDate:   c.now().UTC().Format("2006-01-02T15:04:05"),
	}
	for _, it := range order.Items {
		payload.Items = append(payload.Items, invoiceItem{ID: it.SKU, Quantity: it.Quantity, Price: it.PriceCents})
		payload.InvoiceValue += it.PriceCents * int64(it.Quantity)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode invoice: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build invoice request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("notify marketplace: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusConflict {
		return nil
	}
	return &ports.StatusError{Service: "marketplace", StatusCode: resp.StatusCode}
}

func (c *Client) invoiceURL(order domain.Order) (string, error) {
	u, err := url.Parse(order.MarketplaceEndpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !c.allowed[strings.ToLower(u.Host)] {
		return "", fmt.Errorf("%w: %q", ErrEndpointNotAllowed, order.MarketplaceEndpoint)
	}
	return strings.TrimRight(u.String(), "/") + "/pvt/orders/" + url.PathEscape(order.IdempotencyKey) + "/invoice", nil
}
