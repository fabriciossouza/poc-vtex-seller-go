package app

import (
	"context"
	"fmt"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type SimulateCart struct {
	catalog ports.Catalog
	stock   ports.Stock
	tms     ports.TMSPort
}

func NewSimulateCart(catalog ports.Catalog, stock ports.Stock, tms ports.TMSPort) *SimulateCart {
	return &SimulateCart{catalog: catalog, stock: stock, tms: tms}
}

func (uc *SimulateCart) Execute(ctx context.Context, items []domain.Item) (domain.Simulation, error) {
	quoted := make([]domain.QuotedItem, 0, len(items))
	for _, it := range items {
		price, err := uc.catalog.UnitPriceCents(ctx, it.SKU)
		if err != nil {
			return domain.Simulation{}, fmt.Errorf("price of %s: %w", it.SKU, err)
		}
		level, err := uc.stock.Available(ctx, it.SKU)
		if err != nil {
			return domain.Simulation{}, fmt.Errorf("stock of %s: %w", it.SKU, err)
		}
		quoted = append(quoted, domain.QuoteItem(it, price, level))
	}
	sla, err := uc.tms.QuoteShipping(ctx, items)
	if err != nil {
		return domain.Simulation{}, fmt.Errorf("shipping quote: %w", err)
	}
	return domain.Simulation{Items: quoted, ShippingSLA: sla}, nil
}
