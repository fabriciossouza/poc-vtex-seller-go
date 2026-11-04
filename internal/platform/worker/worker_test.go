package worker_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/resilience"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/worker"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// --- fakes -----------------------------------------------------------------

type memRepo struct {
	mu   sync.Mutex
	byID map[string]domain.Order
}

func (r *memRepo) Save(_ context.Context, o *domain.Order) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[o.ID] = *o
	return nil
}

func (r *memRepo) Get(_ context.Context, id string) (*domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.byID[id]
	if !ok {
		return nil, domain.ErrOrderNotFound
	}
	return &o, nil
}

func (r *memRepo) FindByIdempotencyKey(context.Context, string) (*domain.Order, error) {
	return nil, domain.ErrOrderNotFound
}

type flaky struct {
	err   error
	calls int
}

type fakeERP struct{ flaky }

func (f *fakeERP) SendOrder(context.Context, domain.Order) error { f.calls++; return f.err }

type fakeTMS struct{ flaky }

func (f *fakeTMS) QuoteShipping(context.Context, []domain.Item) (domain.ShippingSLA, error) {
	return domain.ShippingSLA{}, nil
}

func (f *fakeTMS) CreateShipment(context.Context, domain.Order) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return "TRK-1", nil
}

type fakeMarketplace struct{ notifications int }

func (f *fakeMarketplace) NotifyInvoice(context.Context, domain.Order) error {
	f.notifications++
	return nil
}

type fakeConsumer struct {
	mu        sync.Mutex
	queue     []ports.Delivery
	acked     []string
	dlq       []ports.DeadLetter
	ackErr    error
	rejectErr error
	onEmpty   func()
}

func (c *fakeConsumer) push(e domain.Event) {
	c.queue = append(c.queue, ports.Delivery{ID: "d-" + string(rune('a'+len(c.queue))), Event: e})
}

func (c *fakeConsumer) Receive(context.Context) (ports.Delivery, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		if c.onEmpty != nil {
			c.onEmpty()
		}
		return ports.Delivery{}, ports.ErrNoDelivery
	}
	d := c.queue[0]
	c.queue = c.queue[1:]
	return d, nil
}

func (c *fakeConsumer) Ack(_ context.Context, id string) error {
	if c.ackErr != nil {
		return c.ackErr
	}
	c.acked = append(c.acked, id)
	return nil
}

func (c *fakeConsumer) Reject(_ context.Context, d ports.Delivery, reason string, attempts int) error {
	if c.rejectErr != nil {
		return c.rejectErr
	}
	c.dlq = append(c.dlq, ports.DeadLetter{ID: "dl-" + d.ID, Event: d.Event, Reason: reason, Attempts: attempts})
	return nil
}

func (c *fakeConsumer) DeadLetters(context.Context) ([]ports.DeadLetter, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ports.DeadLetter(nil), c.dlq...), nil
}

func (c *fakeConsumer) Discard(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, d := range c.dlq {
		if d.ID == id {
			c.dlq = append(c.dlq[:i], c.dlq[i+1:]...)
			return nil
		}
	}
	return nil
}

type fakeSleeper struct{ slept []time.Duration }

func (s *fakeSleeper) Sleep(_ context.Context, d time.Duration) error {
	s.slept = append(s.slept, d)
	return nil
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

// --- environment -----------------------------------------------------------

type env struct {
	repo     *memRepo
	erp      *fakeERP
	tms      *fakeTMS
	mkt      *fakeMarketplace
	consumer *fakeConsumer
	sleeper  *fakeSleeper
	clock    *clock
	breaker  *resilience.Breaker
	worker   *worker.Worker
}

var (
	erpDown   = &ports.StatusError{Service: "erp", StatusCode: 503}
	erpReject = &ports.StatusError{Service: "erp", StatusCode: 422}
)

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		repo:     &memRepo{byID: map[string]domain.Order{}},
		erp:      &fakeERP{},
		tms:      &fakeTMS{},
		mkt:      &fakeMarketplace{},
		consumer: &fakeConsumer{},
		sleeper:  &fakeSleeper{},
		clock:    &clock{now: t0},
	}
	order, err := domain.NewOrder("ord-1", "mkt-1", []domain.Item{{SKU: "6", Quantity: 1}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	_ = order.Authorize()
	_ = e.repo.Save(context.Background(), order)

	e.breaker = resilience.NewBreaker(resilience.BreakerConfig{FailureThreshold: 3, OpenTimeout: 30 * time.Second}, e.clock.Now)
	retrier := resilience.NewRetrier(resilience.RetryConfig{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: 10 * time.Second}, e.sleeper, func() float64 { return 1 })
	dispatch := app.NewDispatchToERP(e.repo, e.erp, e.tms, e.mkt)
	e.worker = worker.New(e.consumer, dispatch, retrier, e.breaker)
	return e
}

func (e *env) authorized(orderID string) domain.Event {
	return domain.Event{Type: domain.EventOrderAuthorized, OrderID: orderID}
}

func (e *env) status(t *testing.T) domain.OrderStatus {
	t.Helper()
	o, err := e.repo.Get(context.Background(), "ord-1")
	if err != nil {
		t.Fatal(err)
	}
	return o.Status
}

// --- scenarios -------------------------------------------------------------

func TestWorkerDispatchesAnAuthorizedOrderToShippedAndAcks(t *testing.T) {
	e := newEnv(t)
	e.consumer.push(e.authorized("ord-1"))

	if err := e.worker.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if e.status(t) != domain.StatusShipped || len(e.consumer.acked) != 1 || len(e.consumer.dlq) != 0 {
		t.Fatalf("status = %s, acked = %v, dlq = %v", e.status(t), e.consumer.acked, e.consumer.dlq)
	}
}

func TestWorkerWithNothingToReceiveDoesNothing(t *testing.T) {
	e := newEnv(t)
	if err := e.worker.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
}

func TestERPAlwaysDownOpensTheBreakerAndSendsTheMessageToTheDLQ(t *testing.T) {
	e := newEnv(t)
	e.erp.err = erpDown
	e.consumer.push(e.authorized("ord-1"))

	if err := e.worker.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if e.erp.calls != 3 {
		t.Errorf("ERP calls = %d, want 3 (the breaker opens after 3 failures)", e.erp.calls)
	}
	if st := e.breaker.Snapshot().State; st != resilience.StateOpen {
		t.Errorf("breaker = %s, want open", st)
	}
	if len(e.sleeper.slept) != 3 {
		t.Errorf("backoff waits = %v, want 3 (before attempts 2, 3 and 4)", e.sleeper.slept)
	}
	if len(e.consumer.dlq) != 1 || len(e.consumer.acked) != 0 {
		t.Fatalf("dlq = %v, acked = %v", e.consumer.dlq, e.consumer.acked)
	}
	letter := e.consumer.dlq[0]
	if letter.Attempts != 4 || letter.Event != e.authorized("ord-1") ||
		!strings.Contains(letter.Reason, "circuit breaker is open") || !strings.Contains(letter.Reason, "erp responded with status 503") {
		t.Errorf("dead letter = %+v", letter)
	}
	if e.status(t) != domain.StatusAuthorized {
		t.Errorf("status = %s, want authorized (nothing was sent)", e.status(t))
	}
}

func TestPermanentErrorIsNotRetriedAndGoesToTheDLQ(t *testing.T) {
	e := newEnv(t)
	e.erp.err = erpReject
	e.consumer.push(e.authorized("ord-1"))

	_ = e.worker.ProcessNext(context.Background())

	if e.erp.calls != 1 || len(e.sleeper.slept) != 0 {
		t.Errorf("ERP calls = %d, waits = %v; a 4xx must not be retried", e.erp.calls, e.sleeper.slept)
	}
	if len(e.consumer.dlq) != 1 || e.consumer.dlq[0].Attempts != 1 || !strings.Contains(e.consumer.dlq[0].Reason, "422") {
		t.Errorf("dlq = %+v", e.consumer.dlq)
	}
	if st := e.breaker.Snapshot().State; st != resilience.StateClosed {
		t.Errorf("breaker = %s: a 4xx must not trip it", st)
	}
}

func TestReplayAfterTheERPRecoversCompletesTheOrderWithoutResendingToTheERP(t *testing.T) {
	e := newEnv(t)
	e.erp.err = erpDown
	e.consumer.push(e.authorized("ord-1"))
	_ = e.worker.ProcessNext(context.Background())
	e.erp.err = nil
	e.clock.now = e.clock.now.Add(time.Minute)

	res, err := e.worker.ReplayDLQ(context.Background())

	if err != nil || res != (worker.ReplayResult{Replayed: 1}) {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if e.status(t) != domain.StatusShipped || len(e.consumer.dlq) != 0 {
		t.Errorf("status = %s, dlq = %v", e.status(t), e.consumer.dlq)
	}
	if e.erp.calls != 4 {
		t.Errorf("ERP calls = %d, want 3 failures + 1 success", e.erp.calls)
	}
	if st := e.breaker.Snapshot().State; st != resilience.StateClosed {
		t.Errorf("breaker = %s, want closed after the successful probe", st)
	}
}

func TestReplayWhileTheBreakerIsStillOpenKeepsTheLetterInTheDLQ(t *testing.T) {
	e := newEnv(t)
	e.erp.err = erpDown
	e.consumer.push(e.authorized("ord-1"))
	_ = e.worker.ProcessNext(context.Background())
	callsBefore := e.erp.calls

	res, err := e.worker.ReplayDLQ(context.Background())

	if err != nil || res != (worker.ReplayResult{Failed: 1}) {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if len(e.consumer.dlq) != 1 || e.erp.calls != callsBefore {
		t.Errorf("dlq = %v, ERP calls = %d (was %d)", e.consumer.dlq, e.erp.calls, callsBefore)
	}
}

func TestReplayResumesAfterTheStepThatAlreadySucceeded(t *testing.T) {
	e := newEnv(t)
	e.tms.err = &ports.StatusError{Service: "tms", StatusCode: 500}
	e.consumer.push(e.authorized("ord-1"))
	_ = e.worker.ProcessNext(context.Background())
	if e.status(t) != domain.StatusSentToERP {
		t.Fatalf("status = %s, want sent_to_erp", e.status(t))
	}
	e.tms.err = nil
	e.clock.now = e.clock.now.Add(time.Minute)

	if _, err := e.worker.ReplayDLQ(context.Background()); err != nil {
		t.Fatalf("ReplayDLQ: %v", err)
	}

	if e.erp.calls != 1 || e.status(t) != domain.StatusShipped {
		t.Errorf("ERP calls = %d, status = %s; the order was already in the ERP", e.erp.calls, e.status(t))
	}
}

func TestTheSameEventDeliveredTwiceDoesNotRepeatERPOrShipmentEffects(t *testing.T) {
	e := newEnv(t)
	e.consumer.push(e.authorized("ord-1"))
	e.consumer.push(e.authorized("ord-1"))

	_ = e.worker.ProcessNext(context.Background())
	_ = e.worker.ProcessNext(context.Background())

	if e.erp.calls != 1 || e.tms.calls != 1 || e.status(t) != domain.StatusShipped {
		t.Errorf("ERP calls = %d, TMS calls = %d, status = %s", e.erp.calls, e.tms.calls, e.status(t))
	}
	if len(e.consumer.acked) != 2 || len(e.consumer.dlq) != 0 {
		t.Errorf("acked = %v, dlq = %v", e.consumer.acked, e.consumer.dlq)
	}
}

func TestUnsupportedEventTypeGoesStraightToTheDLQ(t *testing.T) {
	e := newEnv(t)
	e.consumer.push(domain.Event{Type: "order.unknown", OrderID: "ord-1"})

	_ = e.worker.ProcessNext(context.Background())

	if len(e.consumer.dlq) != 1 || e.consumer.dlq[0].Attempts != 0 || e.erp.calls != 0 {
		t.Fatalf("dlq = %+v, ERP calls = %d", e.consumer.dlq, e.erp.calls)
	}
}

func TestUnknownOrderIsNotRetriedAndGoesToTheDLQ(t *testing.T) {
	e := newEnv(t)
	e.consumer.push(e.authorized("ghost"))

	_ = e.worker.ProcessNext(context.Background())

	if len(e.consumer.dlq) != 1 || len(e.sleeper.slept) != 0 {
		t.Fatalf("dlq = %+v, waits = %v", e.consumer.dlq, e.sleeper.slept)
	}
}

func TestProcessNextReportsFailuresToSettleTheMessage(t *testing.T) {
	boom := errors.New("redis down")

	ackFails := newEnv(t)
	ackFails.consumer.ackErr = boom
	ackFails.consumer.push(ackFails.authorized("ord-1"))
	if err := ackFails.worker.ProcessNext(context.Background()); !errors.Is(err, boom) {
		t.Errorf("ack failure: err = %v", err)
	}

	rejectFails := newEnv(t)
	rejectFails.erp.err = erpReject
	rejectFails.consumer.rejectErr = boom
	rejectFails.consumer.push(rejectFails.authorized("ord-1"))
	if err := rejectFails.worker.ProcessNext(context.Background()); !errors.Is(err, boom) {
		t.Errorf("reject failure: err = %v", err)
	}
}

func TestRunProcessesMessagesUntilTheContextIsCancelled(t *testing.T) {
	e := newEnv(t)
	e.consumer.push(e.authorized("ord-1"))
	ctx, cancel := context.WithCancel(context.Background())
	e.consumer.onEmpty = cancel

	if err := e.worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if e.status(t) != domain.StatusShipped {
		t.Fatalf("status = %s", e.status(t))
	}
}

func TestShutdownDuringDispatchLeavesTheMessagePendingInsteadOfDeadLettering(t *testing.T) {
	e := newEnv(t)
	e.consumer.push(e.authorized("ord-1"))
	ctx, cancel := context.WithCancel(context.Background())
	e.worker = worker.New(e.consumer, cancelOnCall{cancel: cancel}, resilience.NewRetrier(
		resilience.RetryConfig{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: time.Second}, ctxSleeper{}, nil), e.breaker)

	err := e.worker.ProcessNext(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(e.consumer.dlq) != 0 || len(e.consumer.acked) != 0 {
		t.Fatalf("dlq = %v, acked = %v; an interrupted message must stay pending for redelivery", e.consumer.dlq, e.consumer.acked)
	}
}

type cancelOnCall struct {
	cancel context.CancelFunc
}

func (c cancelOnCall) Execute(context.Context, string) error {
	c.cancel()
	return &ports.StatusError{Service: "erp", StatusCode: 503}
}

type ctxSleeper struct{}

func (ctxSleeper) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func TestShutdownDuringReplayStopsAndKeepsTheLetter(t *testing.T) {
	e := newEnv(t)
	e.consumer.dlq = []ports.DeadLetter{{ID: "dl-1", Event: e.authorized("ord-1"), Reason: "x", Attempts: 4}}
	ctx, cancel := context.WithCancel(context.Background())
	e.worker = worker.New(e.consumer, cancelOnCall{cancel: cancel}, resilience.NewRetrier(
		resilience.RetryConfig{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: time.Second}, ctxSleeper{}, nil), e.breaker)

	_, err := e.worker.ReplayDLQ(ctx)

	if !errors.Is(err, context.Canceled) || len(e.consumer.dlq) != 1 {
		t.Fatalf("err = %v, dlq = %v", err, e.consumer.dlq)
	}
}

type slowFirstDispatch struct{ calls atomic.Int32 }

func (d *slowFirstDispatch) Execute(context.Context, string) error {
	if d.calls.Add(1) == 1 {
		time.Sleep(100 * time.Millisecond) // lets a concurrent replay list the same letter
	}
	return nil
}

func TestConcurrentReplaysNeverDispatchTheSameLetterTwice(t *testing.T) {
	e := newEnv(t)
	e.consumer.dlq = []ports.DeadLetter{{ID: "dl-1", Event: e.authorized("ord-1"), Reason: "x", Attempts: 4}}
	dispatch := &slowFirstDispatch{}
	e.worker = worker.New(e.consumer, dispatch, resilience.NewRetrier(
		resilience.RetryConfig{MaxAttempts: 1}, e.sleeper, nil), e.breaker)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.worker.ReplayDLQ(context.Background()); err != nil {
				t.Errorf("ReplayDLQ: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := dispatch.calls.Load(); n != 1 {
		t.Fatalf("dispatches = %d, want 1", n)
	}
}

func TestReplayDiscardsLettersThatCanNeverSucceedAndKeepsTheRest(t *testing.T) {
	e := newEnv(t)
	failed, _ := domain.NewOrder("ord-failed", "mkt-f", []domain.Item{{SKU: "6", Quantity: 1}}, t0)
	_ = failed.Fail()
	_ = e.repo.Save(context.Background(), failed)
	e.erp.err = erpReject
	e.consumer.dlq = []ports.DeadLetter{
		{ID: "unsupported", Event: domain.Event{Type: "order.unknown", OrderID: "ord-1"}},
		{ID: "no-order-id", Event: domain.Event{}},
		{ID: "ghost", Event: e.authorized("ghost")},
		{ID: "terminal", Event: e.authorized("ord-failed")},
		{ID: "rejected-by-erp", Event: e.authorized("ord-1")},
	}

	res, err := e.worker.ReplayDLQ(context.Background())

	if err != nil || res != (worker.ReplayResult{Discarded: 4, Failed: 1}) {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if len(e.consumer.dlq) != 1 || e.consumer.dlq[0].ID != "rejected-by-erp" {
		t.Fatalf("remaining = %+v: only the ERP rejection deserves a human look", e.consumer.dlq)
	}
}

func TestReplayTreatsALetterOfACancelledOrderAsDone(t *testing.T) {
	e := newEnv(t)
	_ = app.NewCancelOrder(e.repo).Execute(context.Background(), "ord-1")
	e.consumer.dlq = []ports.DeadLetter{{ID: "dl-1", Event: e.authorized("ord-1")}}

	res, err := e.worker.ReplayDLQ(context.Background())

	if err != nil || res != (worker.ReplayResult{Replayed: 1}) || len(e.consumer.dlq) != 0 || e.erp.calls != 0 {
		t.Fatalf("result = %+v, err = %v, dlq = %v, ERP calls = %d", res, err, e.consumer.dlq, e.erp.calls)
	}
}
