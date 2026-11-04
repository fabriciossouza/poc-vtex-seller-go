package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type dispatchEnv struct {
	repo *memRepo
	erp  *fakeERP
	tms  *fakeTMS
	mkt  *fakeMarketplace
	uc   *app.DispatchToERP
}

func newDispatchEnv(t *testing.T) dispatchEnv {
	t.Helper()
	repo := newMemRepo()
	seedOrder(t, repo)
	if err := app.NewFulfillOrder(repo, &fakeBus{}).Execute(context.Background(), "ord-1"); err != nil {
		t.Fatal(err)
	}
	env := dispatchEnv{repo: repo, erp: &fakeERP{}, tms: &fakeTMS{tracking: "TRK-1"}, mkt: &fakeMarketplace{}}
	env.uc = app.NewDispatchToERP(repo, env.erp, env.tms, env.mkt)
	return env
}

func (e dispatchEnv) status(t *testing.T) domain.OrderStatus {
	t.Helper()
	o, err := e.repo.Get(context.Background(), "ord-1")
	if err != nil {
		t.Fatal(err)
	}
	return o.Status
}

func TestDispatchToERPHappyPathReachesShipped(t *testing.T) {
	env := newDispatchEnv(t)

	if err := env.uc.Execute(context.Background(), "ord-1"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	o, _ := env.repo.Get(context.Background(), "ord-1")
	if o.Status != domain.StatusShipped || o.TrackingCode != "TRK-1" {
		t.Fatalf("order = %+v", o)
	}
	if env.mkt.notified["ord-1"] != "TRK-1" {
		t.Fatalf("marketplace notified = %v", env.mkt.notified)
	}
}

func TestDispatchToERPNotifiesMarketplaceWithEndpointAndPrices(t *testing.T) {
	env := newDispatchEnv(t)
	o, _ := env.repo.Get(context.Background(), "ord-1")
	o.MarketplaceEndpoint = "https://marketplace.example/"
	o.Items[0].PriceCents = 2499
	_ = env.repo.Save(context.Background(), o)

	if err := env.uc.Execute(context.Background(), "ord-1"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := env.mkt.last
	if got.MarketplaceEndpoint != "https://marketplace.example/" || got.IdempotencyKey != "key-1" || got.Items[0].PriceCents != 2499 {
		t.Fatalf("notified order = %+v", got)
	}
}

func TestDispatchToERPFailureKeepsOrderAuthorizedAndReturnsError(t *testing.T) {
	env := newDispatchEnv(t)
	boom := errors.New("sap down")
	env.erp.err = boom

	err := env.uc.Execute(context.Background(), "ord-1")

	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped %v", err, boom)
	}
	if got := env.status(t); got != domain.StatusAuthorized {
		t.Fatalf("status = %s, want authorized", got)
	}
	if env.tms.shipments != 0 {
		t.Fatal("TMS must not be called after ERP failure")
	}
}

func TestDispatchToERPTMSFailureIsNotShippedAndRetryDoesNotResendToERP(t *testing.T) {
	env := newDispatchEnv(t)
	boom := errors.New("tms down")
	env.tms.shipmentErr = boom

	err := env.uc.Execute(context.Background(), "ord-1")

	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped %v", err, boom)
	}
	if got := env.status(t); got != domain.StatusSentToERP {
		t.Fatalf("status = %s, want sent_to_erp", got)
	}
	if len(env.mkt.notified) != 0 {
		t.Fatal("marketplace must not be notified")
	}

	env.tms.shipmentErr = nil
	if err := env.uc.Execute(context.Background(), "ord-1"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := env.status(t); got != domain.StatusShipped {
		t.Fatalf("status after retry = %s", got)
	}
	if env.erp.calls != 1 {
		t.Fatalf("ERP calls = %d, want 1", env.erp.calls)
	}
}

func TestDispatchToERPMarketplaceFailureKeepsShipmentAndRetryOnlyRenotifies(t *testing.T) {
	env := newDispatchEnv(t)
	boom := errors.New("marketplace down")
	env.mkt.err = boom

	if err := env.uc.Execute(context.Background(), "ord-1"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	env.mkt.err = nil
	if err := env.uc.Execute(context.Background(), "ord-1"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if env.erp.calls != 1 || env.tms.shipments != 1 {
		t.Fatalf("erp = %d, tms = %d; want 1 and 1", env.erp.calls, env.tms.shipments)
	}
	if env.mkt.notified["ord-1"] != "TRK-1" {
		t.Fatalf("notified = %v", env.mkt.notified)
	}
}

func TestDispatchToERPRejectsOrderThatWasNotAuthorized(t *testing.T) {
	repo := newMemRepo()
	seedOrder(t, repo)
	uc := app.NewDispatchToERP(repo, &fakeERP{}, &fakeTMS{}, &fakeMarketplace{})

	err := uc.Execute(context.Background(), "ord-1")

	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("err = %v", err)
	}
}

func TestDispatchToERPUnknownOrderCallsNoPort(t *testing.T) {
	erp, tms := &fakeERP{}, &fakeTMS{}
	uc := app.NewDispatchToERP(newMemRepo(), erp, tms, &fakeMarketplace{})

	err := uc.Execute(context.Background(), "nope")

	if !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("err = %v", err)
	}
	if erp.calls != 0 || tms.shipments != 0 {
		t.Fatalf("erp = %d, tms = %d; want 0 and 0", erp.calls, tms.shipments)
	}
}

func TestDispatchToERPStopsWhenACancelIsConfirmedDuringTheERPCall(t *testing.T) {
	env := newDispatchEnv(t)
	cancel := app.NewCancelOrder(env.repo, &fakeReserver{})
	env.erp.onSend = func() {
		if err := cancel.Execute(context.Background(), "ord-1"); err != nil {
			t.Errorf("concurrent cancel: %v", err)
		}
	}

	err := env.uc.Execute(context.Background(), "ord-1")

	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := env.status(t); got != domain.StatusCancelled {
		t.Fatalf("status = %s: a confirmed cancellation must not be overwritten", got)
	}
	if env.tms.shipments != 0 || len(env.mkt.notified) != 0 {
		t.Fatalf("shipments = %d, notified = %v; nothing may proceed after the cancel", env.tms.shipments, env.mkt.notified)
	}
}

func TestDispatchToERPReportsAConflictCausedByAnotherWriterSoTheCallerCanRetry(t *testing.T) {
	env := newDispatchEnv(t)
	env.erp.onSend = func() {
		o, _ := env.repo.Get(context.Background(), "ord-1")
		_ = o.MarkSentToERP()
		_ = env.repo.Save(context.Background(), o)
	}

	err := env.uc.Execute(context.Background(), "ord-1")

	if !errors.Is(err, ports.ErrVersionConflict) {
		t.Fatalf("err = %v, want ErrVersionConflict", err)
	}
	env.erp.onSend = nil
	if err := env.uc.Execute(context.Background(), "ord-1"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if env.erp.calls != 1 || env.status(t) != domain.StatusShipped {
		t.Fatalf("erp calls = %d, status = %s", env.erp.calls, env.status(t))
	}
}

func TestDispatchToERPDoesNothingForACancelledOrder(t *testing.T) {
	env := newDispatchEnv(t)
	_ = app.NewCancelOrder(env.repo, &fakeReserver{}).Execute(context.Background(), "ord-1")

	err := env.uc.Execute(context.Background(), "ord-1")

	if !errors.Is(err, ports.ErrNothingDispatched) {
		t.Fatalf("err = %v, want ErrNothingDispatched so the breaker is not fed a call that never happened", err)
	}
	if env.erp.calls != 0 {
		t.Fatalf("ERP calls = %d", env.erp.calls)
	}
}
