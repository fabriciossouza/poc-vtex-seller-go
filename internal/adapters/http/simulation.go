package httpadapter

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

type simulationRequest struct {
	PostalCode string `json:"postalCode"`
	Country    string `json:"country"`
	Items      []struct {
		ID       string `json:"id"`
		Quantity int    `json:"quantity"`
		Seller   string `json:"seller"`
	} `json:"items"`
}

type simulationResponse struct {
	Country       string          `json:"country,omitempty"`
	PostalCode    string          `json:"postalCode,omitempty"`
	Items         []simulatedItem `json:"items"`
	LogisticsInfo []logisticsInfo `json:"logisticsInfo"`
}

type simulatedItem struct {
	ID              string `json:"id"`
	RequestIndex    int    `json:"requestIndex"`
	Quantity        int    `json:"quantity"`
	Seller          string `json:"seller"`
	Price           int64  `json:"price"`
	ListPrice       int64  `json:"listPrice"`
	MeasurementUnit string `json:"measurementUnit"`
	UnitMultiplier  int    `json:"unitMultiplier"`
}

type logisticsInfo struct {
	ItemIndex    int      `json:"itemIndex"`
	Quantity     int      `json:"quantity"`
	ShipsTo      []string `json:"shipsTo,omitempty"`
	StockBalance int      `json:"stockBalance"`
	SLAs         []sla    `json:"slas"`
}

type sla struct {
	ID               string `json:"id"`
	DeliveryChannel  string `json:"deliveryChannel"`
	Name             string `json:"name"`
	ShippingEstimate string `json:"shippingEstimate"`
	Price            int64  `json:"price"`
}

func (h *handler) simulate(w http.ResponseWriter, r *http.Request) {
	var req simulationRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Items) == 0 || len(req.Items) > maxItems {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("between 1 and %d items are required", maxItems))
		return
	}
	items := make([]domain.Item, len(req.Items))
	for i, it := range req.Items {
		if it.ID == "" || it.Quantity <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("item %d needs an id and a positive quantity", i))
			return
		}
		items[i] = domain.Item{SKU: it.ID, Quantity: it.Quantity}
	}

	sim, err := h.Simulate.Execute(r.Context(), items)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	resp := simulationResponse{Country: req.Country, PostalCode: digitsOnly(req.PostalCode)}
	for i, q := range sim.Items {
		resp.Items = append(resp.Items, simulatedItem{
			ID: q.SKU, RequestIndex: i, Quantity: q.Quantity, Seller: req.Items[i].Seller,
			Price: q.UnitPriceCents, ListPrice: q.UnitPriceCents, MeasurementUnit: "un", UnitMultiplier: 1,
		})
		info := logisticsInfo{ItemIndex: i, Quantity: q.Quantity, StockBalance: q.StockOnHand, SLAs: []sla{}}
		if req.Country != "" {
			info.ShipsTo = []string{req.Country}
		}
		if q.Available {
			info.SLAs = append(info.SLAs, sla{
				ID: "standard", DeliveryChannel: "delivery", Name: "Standard",
				ShippingEstimate: formatEstimate(sim.ShippingSLA.Estimate), Price: sim.ShippingSLA.PriceCents,
			})
		}
		resp.LogisticsInfo = append(resp.LogisticsInfo, info)
	}
	writeJSON(w, http.StatusOK, resp)
}

func digitsOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

func formatEstimate(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}
