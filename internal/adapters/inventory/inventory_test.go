package inventory_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/inventory"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

var ctx = context.Background()

func TestCatalogPricesTheFixedSKUsAndTreatsUnknownOnesAsFree(t *testing.T) {
	catalog := inventory.DefaultCatalog()

	price, err := catalog.UnitPriceCents(ctx, "6")
	if err != nil || price != 2499 {
		t.Fatalf("price of 6 = %d, %v; want 2499", price, err)
	}
	if price, err := catalog.UnitPriceCents(ctx, "nope"); err != nil || price != 0 {
		t.Fatalf("price of an unknown sku = %d, %v; want 0 and no error", price, err)
	}
}

func TestCatalogListsItsProductsInSKUOrder(t *testing.T) {
	products := inventory.DefaultCatalog().Products()

	if len(products) < 3 || products[0].SKU != "6" || products[0].Name == "" {
		t.Fatalf("products = %+v", products)
	}
	for i := 1; i < len(products); i++ {
		if products[i-1].SKU >= products[i].SKU {
			t.Fatalf("products not sorted: %+v", products)
		}
	}
}

func TestStockReserveDecrementsAndReleaseGivesTheUnitsBack(t *testing.T) {
	stock := inventory.NewStock(map[string]int{"6": 5})

	if err := stock.Reserve(ctx, "6", 2); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got, _ := stock.Available(ctx, "6"); got != 3 {
		t.Fatalf("available = %d, want 3", got)
	}
	if err := stock.Release(ctx, "6", 2); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got, _ := stock.Available(ctx, "6"); got != 5 {
		t.Fatalf("available = %d, want 5", got)
	}
}

func TestStockRefusesToGoBelowZeroAndKeepsTheLevel(t *testing.T) {
	stock := inventory.NewStock(map[string]int{"6": 1})

	err := stock.Reserve(ctx, "6", 2)

	if !errors.Is(err, ports.ErrInsufficientStock) {
		t.Fatalf("err = %v, want ErrInsufficientStock", err)
	}
	if got, _ := stock.Available(ctx, "6"); got != 1 {
		t.Fatalf("available = %d, want 1", got)
	}
	if err := stock.Reserve(ctx, "unknown", 1); !errors.Is(err, ports.ErrInsufficientStock) {
		t.Fatalf("unknown sku err = %v", err)
	}
}

func TestStockNeverOversellsUnderConcurrentReservations(t *testing.T) {
	stock := inventory.NewStock(map[string]int{"6": 10})
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if stock.Reserve(ctx, "6", 1) == nil {
				won.Add(1)
			}
		}()
	}
	wg.Wait()

	if won.Load() != 10 {
		t.Fatalf("reservations that succeeded = %d, want 10", won.Load())
	}
}

func TestStockResetRestoresTheInitialLevels(t *testing.T) {
	stock := inventory.NewStock(map[string]int{"6": 5, "7": 1})
	_ = stock.Reserve(ctx, "6", 5)

	stock.Reset()

	if levels := stock.Levels(); levels["6"] != 5 || levels["7"] != 1 {
		t.Fatalf("levels = %v", levels)
	}
}

func TestLevelsReturnsACopy(t *testing.T) {
	stock := inventory.NewStock(map[string]int{"6": 5})

	stock.Levels()["6"] = 0

	if got, _ := stock.Available(ctx, "6"); got != 5 {
		t.Fatalf("available = %d, want 5", got)
	}
}
