package httpadapter

import (
	"fmt"
	"math"
	"net/http"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

type placedItem struct {
	ID       string `json:"id"`
	Quantity int    `json:"quantity"`
	Seller   string `json:"seller"`
	Price    int64  `json:"price"`
}

type placementRequest struct {
	MarketplaceOrderID          string       `json:"marketplaceOrderId"`
	MarketplaceServicesEndpoint string       `json:"marketplaceServicesEndpoint"`
	Items                       []placedItem `json:"items"`
	ShippingData                struct {
		LogisticsInfo []struct {
			Price int64 `json:"price"`
		} `json:"logisticsInfo"`
	} `json:"shippingData"`
}

type placementResponse struct {
	MarketplaceOrderID string       `json:"marketplaceOrderId"`
	OrderID            string       `json:"orderId"`
	Items              []placedItem `json:"items"`
}

// placeOrder answers a repeated marketplaceOrderId with the order created the
// first time, which is what makes the marketplace's retries safe.
func (h *handler) placeOrder(w http.ResponseWriter, r *http.Request) {
	var req placementRequest
	if !decode(w, r, &req) {
		return
	}
	if !idPattern.MatchString(req.MarketplaceOrderID) {
		writeError(w, http.StatusBadRequest, "invalid_request", "marketplaceOrderId must be 1 to 40 letters, digits or hyphens")
		return
	}
	if len(req.Items) > maxItems {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("at most %d items are accepted", maxItems))
		return
	}
	if h.ValidateEndpoint == nil || h.ValidateEndpoint(req.MarketplaceServicesEndpoint) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "marketplaceServicesEndpoint is not allowed")
		return
	}
	items := make([]domain.Item, len(req.Items))
	for i, it := range req.Items {
		if !skuPattern.MatchString(it.ID) {
			writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("item %d needs a valid id", i))
			return
		}
		items[i] = domain.Item{SKU: it.ID, Quantity: it.Quantity, PriceCents: it.Price}
	}

	var freight int64
	for _, info := range req.ShippingData.LogisticsInfo {
		if info.Price < 0 || freight > math.MaxInt64-info.Price {
			writeError(w, http.StatusBadRequest, "invalid_request", "logisticsInfo price must be a non-negative amount that fits the order total")
			return
		}
		freight += info.Price
	}

	order, err := h.Place.Execute(r.Context(), app.PlaceOrderCommand{
		OrderID:             h.NewID(),
		IdempotencyKey:      req.MarketplaceOrderID,
		MarketplaceEndpoint: req.MarketplaceServicesEndpoint,
		FreightCents:        freight,
		Items:               items,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	// Built from the stored order: a repeated placement may carry a different
	// body, and the answer must describe the order that actually exists.
	placed := make([]placedItem, len(order.Items))
	for i, it := range order.Items {
		placed[i] = placedItem{ID: it.SKU, Quantity: it.Quantity, Price: it.PriceCents}
		if i < len(req.Items) && req.Items[i].ID == it.SKU {
			placed[i].Seller = req.Items[i].Seller
		}
	}
	writeJSON(w, http.StatusOK, placementResponse{
		MarketplaceOrderID: req.MarketplaceOrderID,
		OrderID:            order.ID,
		Items:              placed,
	})
}
