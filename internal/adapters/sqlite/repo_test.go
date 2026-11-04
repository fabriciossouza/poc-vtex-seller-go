package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/sqlite"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

var createdAt = time.Date(2026, 11, 4, 12, 0, 0, 123, time.UTC)

func openRepo(t *testing.T) *sqlite.Repo {
	t.Helper()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "orders.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func newOrder(t *testing.T, id, key string) *domain.Order {
	t.Helper()
	o, err := domain.NewOrder(id, key, []domain.Item{{SKU: "6", Quantity: 2, PriceCents: 2499}}, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	o.MarketplaceEndpoint = "https://marketplace.example/"
	return o
}

func TestSaveThenGetAndFindByKeyReturnTheSameOrder(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)
	order := newOrder(t, "ord-1", "mkt-1")
	if err := repo.Save(ctx, order); err != nil {
		t.Fatalf("Save: %v", err)
	}

	byID, err := repo.Get(ctx, "ord-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	byKey, err := repo.FindByIdempotencyKey(ctx, "mkt-1")
	if err != nil {
		t.Fatalf("FindByIdempotencyKey: %v", err)
	}

	for _, got := range []*domain.Order{byID, byKey} {
		if got.ID != "ord-1" || got.IdempotencyKey != "mkt-1" || got.Status != domain.StatusReceived ||
			!got.CreatedAt.Equal(createdAt) || got.MarketplaceEndpoint != "https://marketplace.example/" ||
			len(got.Items) != 1 || got.Items[0] != (domain.Item{SKU: "6", Quantity: 2, PriceCents: 2499}) {
			t.Fatalf("got %+v", got)
		}
	}
}

func TestUnknownOrderIsReportedAsNotFound(t *testing.T) {
	repo := openRepo(t)
	if _, err := repo.Get(context.Background(), "nope"); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("Get err = %v", err)
	}
	if _, err := repo.FindByIdempotencyKey(context.Background(), "nope"); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("Find err = %v", err)
	}
}

func TestSaveUpdatesStatusAndTrackingOfAnExistingOrder(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)
	order := newOrder(t, "ord-1", "mkt-1")
	_ = repo.Save(ctx, order)
	_ = order.Authorize()
	_ = order.MarkSentToERP()
	_ = order.MarkShipped("TRK-1")

	if err := repo.Save(ctx, order); err != nil {
		t.Fatalf("Save update: %v", err)
	}

	got, _ := repo.Get(ctx, "ord-1")
	if got.Status != domain.StatusShipped || got.TrackingCode != "TRK-1" {
		t.Fatalf("got %+v", got)
	}
}

func TestConcurrentSavesWithTheSameKeyYieldASingleOrder(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)
	const workers = 20

	var wg sync.WaitGroup
	saved := make([]*domain.Order, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			saved[i] = newOrder(t, "ord-"+string(rune('a'+i)), "mkt-1")
			<-start
			errs[i] = repo.Save(ctx, saved[i])
		}()
	}
	close(start)
	wg.Wait()

	winner := saved[0].ID
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if saved[i].ID != winner {
			t.Fatalf("worker %d got order %s, want the existing %s", i, saved[i].ID, winner)
		}
	}
	stored, err := repo.FindByIdempotencyKey(ctx, "mkt-1")
	if err != nil || stored.ID != winner {
		t.Fatalf("stored = %+v, err = %v; want order %s", stored, err, winner)
	}
}

func TestOrdersSurviveReopeningTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.db")
	first, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Save(context.Background(), newOrder(t, "ord-1", "mkt-1"))
	_ = first.Close()

	second, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if _, err := second.Get(context.Background(), "ord-1"); err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
}

func TestSaveRejectsAStaleVersionAndKeepsTheStoredOrder(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)
	_ = repo.Save(ctx, newOrder(t, "ord-1", "mkt-1"))
	worker, _ := repo.Get(ctx, "ord-1")
	canceller, _ := repo.Get(ctx, "ord-1")
	_ = canceller.Cancel()
	if err := repo.Save(ctx, canceller); err != nil {
		t.Fatalf("first writer: %v", err)
	}
	_ = worker.Authorize()

	err := repo.Save(ctx, worker)

	if !errors.Is(err, ports.ErrVersionConflict) {
		t.Fatalf("err = %v, want ErrVersionConflict", err)
	}
	got, _ := repo.Get(ctx, "ord-1")
	if got.Status != domain.StatusCancelled {
		t.Fatalf("status = %s: the stale write must not win", got.Status)
	}
}

func TestSaveBumpsTheVersionSoTheSameOrderCanBeSavedAgain(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)
	order := newOrder(t, "ord-1", "mkt-1")
	_ = repo.Save(ctx, order)

	_ = order.Authorize()
	if err := repo.Save(ctx, order); err != nil {
		t.Fatalf("first update: %v", err)
	}
	_ = order.MarkSentToERP()
	if err := repo.Save(ctx, order); err != nil {
		t.Fatalf("second update: %v", err)
	}

	got, _ := repo.Get(ctx, "ord-1")
	if got.Status != domain.StatusSentToERP || got.Version != 2 {
		t.Fatalf("got status %s version %d", got.Status, got.Version)
	}
}

func TestResetRemovesEveryOrderAndFreesTheIdempotencyKeys(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)
	if err := repo.Save(ctx, newOrder(t, "ord-1", "mkt-1")); err != nil {
		t.Fatal(err)
	}

	if err := repo.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if _, err := repo.Get(ctx, "ord-1"); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("Get after Reset = %v, want ErrOrderNotFound", err)
	}
	again := newOrder(t, "ord-2", "mkt-1")
	if err := repo.Save(ctx, again); err != nil || again.ID != "ord-2" {
		t.Fatalf("Save with the old key = %v, id %s", err, again.ID)
	}
}

func TestSaveOfAnUpdatedOrderDoesNotResurrectItAfterAReset(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)
	order := newOrder(t, "ord-1", "mkt-1")
	if err := repo.Save(ctx, order); err != nil {
		t.Fatal(err)
	}
	_ = order.Authorize()
	if err := repo.Reset(ctx); err != nil {
		t.Fatal(err)
	}

	err := repo.Save(ctx, order)

	if !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("err = %v, want ErrOrderNotFound", err)
	}
	if _, err := repo.Get(ctx, "ord-1"); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("the order came back after the reset: %v", err)
	}
}

func TestSaveInsertsOnlyBrandNewReceivedOrders(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)
	advanced := newOrder(t, "ord-2", "mkt-2")
	_ = advanced.Authorize()

	if err := repo.Save(ctx, advanced); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("an authorized order that was never stored: err = %v, want ErrOrderNotFound", err)
	}
	versioned := newOrder(t, "ord-3", "mkt-3")
	versioned.Version = 4
	if err := repo.Save(ctx, versioned); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("a versioned order that was never stored: err = %v, want ErrOrderNotFound", err)
	}
}

func TestSaveWithAnIDOfAnotherOrderFailsAndLeavesTheStoredOrderUntouched(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)
	first := newOrder(t, "ord-1", "mkt-1")
	if err := repo.Save(ctx, first); err != nil {
		t.Fatal(err)
	}

	err := repo.Save(ctx, newOrder(t, "ord-1", "mkt-OTHER"))

	if err == nil || errors.Is(err, ports.ErrVersionConflict) {
		t.Fatalf("err = %v, want an id collision error", err)
	}
	got, _ := repo.Get(ctx, "ord-1")
	if got.IdempotencyKey != "mkt-1" || got.Status != domain.StatusReceived || got.Version != 0 {
		t.Fatalf("stored order was changed: %+v", got)
	}
}

func TestFreightSurvivesSaveAndGet(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)
	order := newOrder(t, "ord-1", "mkt-1")
	order.FreightCents = 1099
	if err := repo.Save(ctx, order); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := repo.Get(ctx, "ord-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.FreightCents != 1099 {
		t.Fatalf("FreightCents = %d, want 1099", got.FreightCents)
	}
}
