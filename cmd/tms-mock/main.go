// Command tms-mock is a stand-in for the carrier system: shipping quotes and
// shipments with a tracking code. It follows docs/contratos-internos.md.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type item struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type server struct {
	mu        sync.Mutex
	shipments map[string]string
}

func newHandler() http.Handler {
	s := &server{shipments: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /quotes", s.quote)
	mux.HandleFunc("POST /shipments", s.ship)
	mux.HandleFunc("GET /admin/shipments", s.count)
	mux.HandleFunc("POST /admin/reset", s.reset)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

func (s *server) quote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Items []item `json:"items"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "items are required"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"priceCents": 1099, "estimateHours": 168})
}

func (s *server) ship(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key is required"})
		return
	}
	var req struct {
		Items []item `json:"items"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	code, ok := s.shipments[key]
	if !ok {
		sum := sha256.Sum256([]byte(key))
		code = "TRK-" + strings.ToUpper(hex.EncodeToString(sum[:4]))
		s.shipments[key] = code
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"trackingCode": code})
}

func (s *server) count(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]int{"count": len(s.shipments)})
}

func (s *server) reset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.shipments = map[string]string{}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"reset": true})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	srv := &http.Server{
		Addr: addr, Handler: newHandler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}
	logger.Info("tms-mock listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("tms-mock stopped", "error", err)
		os.Exit(1)
	}
}
