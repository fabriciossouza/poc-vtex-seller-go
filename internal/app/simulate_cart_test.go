package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

func TestSimulateCartDeniesItemWithoutStock(t *testing.T) {
	uc := app.NewSimulateCart(
		fakeCatalog{prices: map[string]int64{"in": 1000, "out": 500}},
		fakeStock{levels: map[string]int{"in": 10, "out": 0}},
		&fakeTMS{sla: domain.ShippingSLA{PriceCents: 700, Estimate: 48 * time.Hour}},
	)

	sim, err := uc.Execute(context.Background(), []domain.Item{
		{SKU: "in", Quantity: 2},
		{SKU: "out", Quantity: 1},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !sim.Items[0].Available || sim.Items[0].UnitPriceCents != 1000 {
		t.Fatalf("in-stock item = %+v", sim.Items[0])
	}
	if sim.Items[1].Available {
		t.Fatal("out-of-stock item must be denied")
	}
	if sim.Purchasable() {
		t.Fatal("simulation must not be purchasable")
	}
	if sim.ShippingSLA.PriceCents != 700 {
		t.Fatalf("SLA = %+v", sim.ShippingSLA)
	}
}

type failingCatalog struct{ err error }

func (f failingCatalog) UnitPriceCents(context.Context, string) (int64, error) { return 0, f.err }

type failingStock struct{ err error }

func (f failingStock) Available(context.Context, string) (int, error) { return 0, f.err }

type failingQuoteTMS struct {
	fakeTMS
	err error
}

func (f *failingQuoteTMS) QuoteShipping(context.Context, []domain.Item) (domain.ShippingSLA, error) {
	return domain.ShippingSLA{}, f.err
}

func TestSimulateCartPropagatesPortErrors(t *testing.T) {
	boom := errors.New("boom")
	items := []domain.Item{{SKU: "a", Quantity: 1}}
	okCatalog := fakeCatalog{prices: map[string]int64{"a": 1}}
	okStock := fakeStock{levels: map[string]int{"a": 1}}

	cases := map[string]*app.SimulateCart{
		"catalog":  app.NewSimulateCart(failingCatalog{boom}, okStock, &fakeTMS{}),
		"stock":    app.NewSimulateCart(okCatalog, failingStock{boom}, &fakeTMS{}),
		"shipping": app.NewSimulateCart(okCatalog, okStock, &failingQuoteTMS{err: boom}),
	}
	for name, uc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := uc.Execute(context.Background(), items); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want wrapped %v", err, boom)
			}
		})
	}
}
