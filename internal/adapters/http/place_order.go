package httpadapter

import (
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
	if req.MarketplaceOrderID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "marketplaceOrderId is required")
		return
	}
	items := make([]domain.Item, len(req.Items))
	for i, it := range req.Items {
		items[i] = domain.Item{SKU: it.ID, Quantity: it.Quantity, PriceCents: it.Price}
	}

	order, err := h.Place.Execute(r.Context(), app.PlaceOrderCommand{
		OrderID:             h.NewID(),
		IdempotencyKey:      req.MarketplaceOrderID,
		MarketplaceEndpoint: req.MarketplaceServicesEndpoint,
		Items:               items,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, placementResponse{
		MarketplaceOrderID: req.MarketplaceOrderID,
		OrderID:            order.ID,
		Items:              req.Items,
	})
}
