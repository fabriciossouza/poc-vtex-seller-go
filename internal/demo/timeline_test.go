package demo_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/demo"
)

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func newTimeline() *demo.Timeline {
	return demo.NewTimeline(func() time.Time { return t0 }, 3, 4)
}

func TestTimelineKeepsEventsInOrderPerMarketplaceOrder(t *testing.T) {
	tl := newTimeline()

	tl.Add("mkt-1", "marketplace", demo.Info, "pedido enviado")
	tl.Add("mkt-1", "erp", demo.Error, "falhou")
	tl.Add("mkt-2", "marketplace", demo.Info, "outro")

	got := tl.Events("mkt-1")
	if len(got) != 2 || got[0].Message != "pedido enviado" || got[1].Level != demo.Error || !got[0].At.Equal(t0) {
		t.Fatalf("events = %+v", got)
	}
	if len(tl.Events("mkt-2")) != 1 {
		t.Fatal("orders must not share events")
	}
}

func TestTimelineResolvesSellerOrderIDsThroughAliases(t *testing.T) {
	tl := newTimeline()
	tl.Alias("ord-9", "mkt-1")

	tl.Add("ord-9", "fila", demo.Info, "evento publicado")

	if got := tl.Events("mkt-1"); len(got) != 1 || got[0].Source != "fila" {
		t.Fatalf("events = %+v", got)
	}
	if id, ok := tl.SellerOrderID("mkt-1"); !ok || id != "ord-9" {
		t.Fatalf("SellerOrderID = %q, %v", id, ok)
	}
}

func TestTimelineDropsTheOldestOrderAndItsAliasWhenFull(t *testing.T) {
	tl := newTimeline() // 3 orders
	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("mkt-%d", i)
		tl.Alias(fmt.Sprintf("ord-%d", i), id)
		tl.Add(id, "marketplace", demo.Info, "x")
	}

	if len(tl.Events("mkt-1")) != 0 {
		t.Fatal("oldest order should be gone")
	}
	if _, ok := tl.SellerOrderID("mkt-1"); ok {
		t.Fatal("its alias should be gone too")
	}
	if got := tl.Orders(); len(got) != 3 || got[0] != "mkt-4" || got[2] != "mkt-2" {
		t.Fatalf("orders = %v, want newest first", got)
	}
}

func TestTimelineCapsEventsPerOrderKeepingTheNewest(t *testing.T) {
	tl := newTimeline() // 4 events per order
	for i := 1; i <= 6; i++ {
		tl.Add("mkt-1", "erp", demo.Info, fmt.Sprint(i))
	}

	got := tl.Events("mkt-1")
	if len(got) != 4 || got[0].Message != "3" || got[3].Message != "6" {
		t.Fatalf("events = %+v", got)
	}
}

func TestTimelineTruncatesVeryLongMessages(t *testing.T) {
	tl := newTimeline()

	tl.Add("mkt-1", "erp", demo.Error, string(make([]byte, 5000)))

	if n := len(tl.Events("mkt-1")[0].Message); n > 300 {
		t.Fatalf("message length = %d", n)
	}
}

func TestTimelineResetForgetsEverything(t *testing.T) {
	tl := newTimeline()
	tl.Alias("ord-1", "mkt-1")
	tl.Add("mkt-1", "erp", demo.Info, "x")

	tl.Reset()

	if len(tl.Orders()) != 0 || len(tl.Events("mkt-1")) != 0 {
		t.Fatal("timeline was not emptied")
	}
	if _, ok := tl.SellerOrderID("mkt-1"); ok {
		t.Fatal("aliases must be forgotten")
	}
}
