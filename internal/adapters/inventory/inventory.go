// Package inventory holds the in-memory catalog and stock of the demo: a few
// fictional SKUs, with stock that placements can reserve and release.
package inventory

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type Product struct {
	SKU        string
	Name       string
	PriceCents int64
}

type Catalog struct {
	products map[string]Product
}

func NewCatalog(products ...Product) *Catalog {
	c := &Catalog{products: make(map[string]Product, len(products))}
	for _, p := range products {
		c.products[p.SKU] = p
	}
	return c
}

// DefaultCatalog is the fixed set of fictional SKUs used by the demo. SKU 6
// and its price match the payloads in testdata.
func DefaultCatalog() *Catalog {
	return NewCatalog(
		Product{SKU: "6", Name: "Camiseta basica", PriceCents: 2499},
		Product{SKU: "7", Name: "Caneca de ceramica", PriceCents: 3290},
		Product{SKU: "8", Name: "Mochila urbana", PriceCents: 18990},
		Product{SKU: "9", Name: "Tenis edicao limitada", PriceCents: 45990},
	)
}

// UnitPriceCents answers 0 for an unknown sku: the stock side reports it as
// unavailable, so the marketplace simply sees an item it cannot buy.
func (c *Catalog) UnitPriceCents(_ context.Context, sku string) (int64, error) {
	return c.products[sku].PriceCents, nil
}

func (c *Catalog) Products() []Product {
	return slices.SortedFunc(maps.Values(c.products), func(a, b Product) int {
		return strings.Compare(a.SKU, b.SKU)
	})
}

type Stock struct {
	initial map[string]int

	mu     sync.Mutex
	levels map[string]int
}

func NewStock(initial map[string]int) *Stock {
	return &Stock{initial: maps.Clone(initial), levels: maps.Clone(initial)}
}

// DefaultStock matches DefaultCatalog; SKU 9 starts sold out.
func DefaultStock() *Stock {
	return NewStock(map[string]int{"6": 50, "7": 30, "8": 10, "9": 0})
}

func (s *Stock) Available(_ context.Context, sku string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.levels[sku], nil
}

func (s *Stock) Reserve(_ context.Context, sku string, quantity int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.levels[sku] < quantity {
		return fmt.Errorf("sku %s: %w", sku, ports.ErrInsufficientStock)
	}
	s.levels[sku] -= quantity
	return nil
}

func (s *Stock) Release(_ context.Context, sku string, quantity int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.levels[sku] += quantity
	return nil
}

func (s *Stock) Levels() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.levels)
}

func (s *Stock) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.levels = maps.Clone(s.initial)
}
