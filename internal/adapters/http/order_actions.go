package httpadapter

import "net/http"

type receiptResponse struct {
	Date               string `json:"date"`
	MarketplaceOrderID string `json:"marketplaceOrderId"`
	OrderID            string `json:"orderId"`
	Receipt            string `json:"receipt"`
}

// fulfillRequest keeps only marketplaceOrderId: the official schema reuses
// the cancellation payload, but the documented text sends just that field.
type fulfillRequest struct {
	MarketplaceOrderID string `json:"marketplaceOrderId"`
}

type cancelRequest struct {
	MarketplaceOrderID      string `json:"marketplaceOrderId"`
	MarketplaceOrderGroup   string `json:"marketplaceOrderGroup"`
	CancellationRequestID   string `json:"cancellationRequestId"`
	CancellationRequestDate string `json:"cancellationRequestDate"`
	Reason                  string `json:"reason"`
	RequestedByUser         *bool  `json:"requestedByUser"`
}

func (h *handler) fulfill(w http.ResponseWriter, r *http.Request) {
	var req fulfillRequest
	if !decode(w, r, &req) {
		return
	}
	if !idPattern.MatchString(req.MarketplaceOrderID) {
		writeError(w, http.StatusBadRequest, "invalid_request", "marketplaceOrderId must be 1 to 40 letters, digits or hyphens")
		return
	}
	h.act(w, r, h.Fulfill, req.MarketplaceOrderID)
}

func (h *handler) cancel(w http.ResponseWriter, r *http.Request) {
	var req cancelRequest
	if !decode(w, r, &req) {
		return
	}
	if !idPattern.MatchString(req.MarketplaceOrderID) || req.MarketplaceOrderGroup == "" || req.CancellationRequestID == "" ||
		req.CancellationRequestDate == "" || req.Reason == "" || req.RequestedByUser == nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"marketplaceOrderId, marketplaceOrderGroup, cancellationRequestId, cancellationRequestDate, reason and requestedByUser are required")
		return
	}
	h.act(w, r, h.Cancel, req.MarketplaceOrderID)
}

func (h *handler) act(w http.ResponseWriter, r *http.Request, action OrderAction, marketplaceOrderID string) {
	orderID := r.PathValue("sellerOrderId")
	if err := action.Execute(r.Context(), orderID); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receiptResponse{
		Date:               h.Clock.Now().UTC().Format("2006-01-02 15:04:05"),
		MarketplaceOrderID: marketplaceOrderID,
		OrderID:            orderID,
		Receipt:            h.NewID(),
	})
}
