package bootstrap

import (
	"context"
	"fmt"
	"strings"

	httpadapter "github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/http"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/demo"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

// recordingPlacer links the seller order id to the marketplace order id in the
// timeline at the placement itself, so a placement made straight on
// /pvt/orders shows up as one order, not as two half entries.
type recordingPlacer struct {
	next httpadapter.OrderPlacer
	tl   *demo.Timeline
}

func (p recordingPlacer) Execute(ctx context.Context, cmd app.PlaceOrderCommand) (*domain.Order, error) {
	_, repeated := p.tl.SellerOrderID(cmd.IdempotencyKey)
	order, err := p.next.Execute(ctx, cmd)
	if err != nil {
		return nil, err
	}
	p.tl.Alias(order.ID, cmd.IdempotencyKey)
	if repeated {
		p.tl.Add(cmd.IdempotencyKey, "marketplace", demo.Info, "pedido repetido: o seller devolveu o mesmo orderId, sem criar outro pedido")
		return order, nil
	}
	lines := make([]string, len(order.Items))
	for i, it := range order.Items {
		lines[i] = fmt.Sprintf("%dx SKU %s", it.Quantity, it.SKU)
	}
	p.tl.Add(cmd.IdempotencyKey, "marketplace", demo.Info, fmt.Sprintf("pedido enviado ao seller (%s), orderId %s", strings.Join(lines, ", "), order.ID))
	return order, nil
}
