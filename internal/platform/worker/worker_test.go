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
	// receiveScript is consumed one entry per Receive before the queue is
	// read; a non-nil entry is returned as the Receive error.
	receiveScript []error
}

func (c *fakeConsumer) push(e domain.Event) {
	c.queue = append(c.queue, ports.Delivery{ID: "d-" + string(rune('a'+len(c.queue))), Event: e})
}

func (c *fakeConsumer) Receive(context.Context) (ports.Delivery, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.receiveScript) > 0 {
		err := c.receiveScript[0]
		c.receiveScript = c.receiveScript[1:]
		if err != nil {
			return ports.Delivery{}, err
		}
	}
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
	_ = app.NewCancelOrder(e.repo, noStock{}).Execute(context.Background(), "ord-1", "mkt-1")
	e.consumer.dlq = []ports.DeadLetter{{ID: "dl-1", Event: e.authorized("ord-1")}}

	res, err := e.worker.ReplayDLQ(context.Background())

	if err != nil || res != (worker.ReplayResult{Replayed: 1}) || len(e.consumer.dlq) != 0 || e.erp.calls != 0 {
		t.Fatalf("result = %+v, err = %v, dlq = %v, ERP calls = %d", res, err, e.consumer.dlq, e.erp.calls)
	}
}

func TestDuplicateEventResendsTheInvoiceOnceMoreWhichTheMarketplaceClientToleratesWith409(t *testing.T) {
	e := newEnv(t)
	e.consumer.push(e.authorized("ord-1"))
	e.consumer.push(e.authorized("ord-1"))

	_ = e.worker.ProcessNext(context.Background())
	_ = e.worker.ProcessNext(context.Background())

	// The order is already shipped on the second delivery, but the invoice
	// notification is the last step and keeps no state, so it is sent again.
	// Accepted: marketplace.Client treats 409 (same invoice number) as success.
	if e.mkt.notifications != 2 || e.erp.calls != 1 || e.tms.calls != 1 {
		t.Fatalf("notifications = %d, ERP calls = %d, TMS calls = %d", e.mkt.notifications, e.erp.calls, e.tms.calls)
	}
}

var errQueueDown = errors.New("redis: connection refused")

// runWithBackoff builds a worker whose queue-error pauses are recorded in
// the returned sleeper instead of being waited for.
func runWithBackoff(e *env, sleep func(time.Duration) error) *worker.Worker {
	dispatch := app.NewDispatchToERP(e.repo, e.erp, e.tms, e.mkt)
	retrier := resilience.NewRetrier(resilience.RetryConfig{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: 10 * time.Second}, e.sleeper, func() float64 { return 1 })
	return worker.New(e.consumer, dispatch, retrier, e.breaker,
		worker.WithBackoff(sleeperFunc(sleep), 100*time.Millisecond, 400*time.Millisecond))
}

type sleeperFunc func(time.Duration) error

func (f sleeperFunc) Sleep(_ context.Context, d time.Duration) error { return f(d) }

func TestRunSurvivesQueueErrorsBackingOffAndStillProcessesTheMessage(t *testing.T) {
	e := newEnv(t)
	e.consumer.receiveScript = []error{errQueueDown, errQueueDown, errQueueDown, errQueueDown, nil}
	e.consumer.push(e.authorized("ord-1"))
	ctx, cancel := context.WithCancel(context.Background())
	e.consumer.onEmpty = cancel
	var slept []time.Duration
	w := runWithBackoff(e, func(d time.Duration) error { slept = append(slept, d); return nil })

	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if e.status(t) != domain.StatusShipped {
		t.Fatalf("status = %s", e.status(t))
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond}
	if len(slept) != len(want) {
		t.Fatalf("backoff = %v, want %v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Fatalf("backoff = %v, want %v", slept, want)
		}
	}
}

func TestRunBackoffRestartsAfterASuccessfulReceive(t *testing.T) {
	e := newEnv(t)
	e.consumer.receiveScript = []error{errQueueDown, errQueueDown, nil, errQueueDown}
	e.consumer.push(e.authorized("ord-1"))
	ctx, cancel := context.WithCancel(context.Background())
	e.consumer.onEmpty = cancel
	var slept []time.Duration
	w := runWithBackoff(e, func(d time.Duration) error { slept = append(slept, d); return nil })

	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 100 * time.Millisecond}
	if len(slept) != len(want) || slept[2] != want[2] {
		t.Fatalf("backoff = %v, want %v", slept, want)
	}
}

func TestRunKeepsGoingWhenAckFails(t *testing.T) {
	e := newEnv(t)
	e.consumer.ackErr = errQueueDown
	e.consumer.push(e.authorized("ord-1"))
	ctx, cancel := context.WithCancel(context.Background())
	e.consumer.onEmpty = cancel
	var slept []time.Duration
	w := runWithBackoff(e, func(d time.Duration) error { slept = append(slept, d); return nil })

	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(slept) != 1 {
		t.Fatalf("backoff = %v, want one pause after the failed ack", slept)
	}
}

func TestRunKeepsGoingWhenRejectFails(t *testing.T) {
	e := newEnv(t)
	e.erp.err = erpReject
	e.consumer.rejectErr = errQueueDown
	e.consumer.push(e.authorized("ord-1"))
	ctx, cancel := context.WithCancel(context.Background())
	e.consumer.onEmpty = cancel
	var slept []time.Duration
	w := runWithBackoff(e, func(d time.Duration) error { slept = append(slept, d); return nil })

	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(slept) != 1 {
		t.Fatalf("backoff = %v, want one pause after the failed reject", slept)
	}
}

func TestRunReturnsNilWhenShutdownInterruptsTheBackoff(t *testing.T) {
	e := newEnv(t)
	e.consumer.receiveScript = []error{errQueueDown}
	ctx, cancel := context.WithCancel(context.Background())
	w := runWithBackoff(e, func(time.Duration) error { cancel(); return ctx.Err() })

	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil on a normal shutdown", err)
	}
}

type noStock struct{}

func (noStock) Reserve(context.Context, string, int) error { return nil }
func (noStock) Release(context.Context, string, int) error { return nil }

type stubDispatch struct {
	mu     sync.Mutex
	err    error
	active map[string]int
	peak   map[string]int
	enter  chan string
	gate   chan struct{}
}

func (d *stubDispatch) Execute(_ context.Context, orderID string) error {
	d.mu.Lock()
	if d.active == nil {
		d.active, d.peak = map[string]int{}, map[string]int{}
	}
	d.active[orderID]++
	d.peak[orderID] = max(d.peak[orderID], d.active[orderID])
	d.mu.Unlock()
	if d.enter != nil {
		d.enter <- orderID
	}
	if d.gate != nil {
		<-d.gate
	}
	d.mu.Lock()
	d.active[orderID]--
	d.mu.Unlock()
	return d.err
}

func stubWorker(t *testing.T, e *env, d worker.Dispatcher) *worker.Worker {
	t.Helper()
	retrier := resilience.NewRetrier(resilience.RetryConfig{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: 10 * time.Second}, e.sleeper, func() float64 { return 1 })
	return worker.New(e.consumer, d, retrier, e.breaker)
}

func TestVersionConflictsAreRetriedButDoNotOpenTheBreaker(t *testing.T) {
	e := newEnv(t)
	w := stubWorker(t, e, &stubDispatch{err: ports.ErrVersionConflict})
	e.consumer.push(e.authorized("ord-1"))

	if err := w.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}

	if snap := e.breaker.Snapshot(); snap.State != resilience.StateClosed || snap.ConsecutiveFailures != 0 {
		t.Fatalf("breaker = %+v: contention in our own database says nothing about the ERP", snap)
	}
	if len(e.consumer.dlq) != 1 || len(e.sleeper.slept) != 4 {
		t.Fatalf("dlq = %d, waits = %d; want the retries to run and the letter to be parked", len(e.consumer.dlq), len(e.sleeper.slept))
	}
}

func TestARunThatCalledNothingDoesNotCloseAHalfOpenBreaker(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		_ = e.breaker.Execute(func() error { return erpDown })
	}
	e.clock.now = e.clock.now.Add(time.Minute)
	w := stubWorker(t, e, &stubDispatch{err: ports.ErrNothingDispatched})
	e.consumer.push(e.authorized("ord-1"))

	if err := w.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}

	if st := e.breaker.Snapshot().State; st != resilience.StateHalfOpen {
		t.Fatalf("breaker = %s, want half-open: a cancelled order proves nothing about the ERP", st)
	}
	if len(e.consumer.acked) != 1 || len(e.consumer.dlq) != 0 {
		t.Fatalf("acked = %v, dlq = %v: the message is done", e.consumer.acked, e.consumer.dlq)
	}
}

func TestProcessNextAndReplayNeverDispatchTheSameOrderInParallel(t *testing.T) {
	e := newEnv(t)
	d := &stubDispatch{enter: make(chan string, 4), gate: make(chan struct{})}
	w := stubWorker(t, e, d)
	e.consumer.push(e.authorized("ord-1"))
	e.consumer.dlq = []ports.DeadLetter{{ID: "dl-1", Event: e.authorized("ord-1")}}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = w.ProcessNext(context.Background()) }()
	<-d.enter // the first dispatch of ord-1 is in flight
	go func() { defer wg.Done(); _, _ = w.ReplayDLQ(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // give the replay time to (wrongly) enter
	close(d.gate)
	wg.Wait()

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.peak["ord-1"] != 1 {
		t.Fatalf("peak concurrent dispatches of ord-1 = %d, want 1", d.peak["ord-1"])
	}
}

func TestReplayStopsAtTheBatchLimitAndLeavesTheRestInTheDLQ(t *testing.T) {
	e := newEnv(t)
	retrier := resilience.NewRetrier(resilience.RetryConfig{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: time.Second}, e.sleeper, func() float64 { return 1 })
	w := worker.New(e.consumer, &stubDispatch{}, retrier, e.breaker, worker.WithReplayBatch(2))
	for _, id := range []string{"a", "b", "c", "d"} {
		e.consumer.dlq = append(e.consumer.dlq, ports.DeadLetter{ID: "dl-" + id, Event: e.authorized("ord-" + id)})
	}

	res, err := w.ReplayDLQ(context.Background())

	if err != nil || res.Replayed != 2 || len(e.consumer.dlq) != 2 {
		t.Fatalf("res = %+v, err = %v, left = %d; want 2 replayed and 2 left", res, err, len(e.consumer.dlq))
	}
}
