package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

const releaseTimeout = 5 * time.Second

// releaseStock gives reserved units back. It runs on its own context: the
// caller may have hung up, and the units must not stay reserved because of it.
func releaseStock(ctx context.Context, stock ports.StockReserver, items []domain.Item) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	var errs []error
	for _, it := range items {
		if err := stock.Release(ctx, it.SKU, it.Quantity); err != nil {
			errs = append(errs, fmt.Errorf("release %s: %w", it.SKU, err))
		}
	}
	return errors.Join(errs...)
}
