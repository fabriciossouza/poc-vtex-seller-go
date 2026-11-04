package resilience_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/resilience"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newBreaker(c *fakeClock) *resilience.Breaker {
	return resilience.NewBreaker(resilience.BreakerConfig{FailureThreshold: 3, OpenTimeout: 10 * time.Second}, c.Now)
}

func fail(b *resilience.Breaker) error {
	return b.Execute(func() error { return transientErr{true} })
}

func succeed(b *resilience.Breaker) error { return b.Execute(func() error { return nil }) }

func TestBreakerOpensAfterConsecutiveTransientFailuresAndRejectsCalls(t *testing.T) {
	b := newBreaker(&fakeClock{now: time.Unix(0, 0)})
	for i := 0; i < 3; i++ {
		_ = fail(b)
	}

	called := false
	err := b.Execute(func() error { called = true; return nil })

	if !errors.Is(err, resilience.ErrCircuitOpen) || called {
		t.Fatalf("err = %v, called = %v; want ErrCircuitOpen without calling", err, called)
	}
	if snap := b.Snapshot(); snap.State != resilience.StateOpen || snap.ConsecutiveFailures != 3 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestBreakerSuccessResetsTheFailureCount(t *testing.T) {
	b := newBreaker(&fakeClock{now: time.Unix(0, 0)})
	_ = fail(b)
	_ = fail(b)
	_ = succeed(b)
	_ = fail(b)
	_ = fail(b)

	if st := b.Snapshot().State; st != resilience.StateClosed {
		t.Fatalf("state = %s, want closed", st)
	}
}

func TestBreakerIgnoresPermanentErrorsForTripping(t *testing.T) {
	b := newBreaker(&fakeClock{now: time.Unix(0, 0)})
	for i := 0; i < 5; i++ {
		_ = b.Execute(func() error { return transientErr{false} })
	}

	if st := b.Snapshot().State; st != resilience.StateClosed {
		t.Fatalf("state = %s: a 4xx proves the dependency is alive", st)
	}
}

func TestBreakerHalfOpenLetsOneProbeThroughAndClosesOnSuccess(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	b := newBreaker(clock)
	for i := 0; i < 3; i++ {
		_ = fail(b)
	}
	clock.Advance(10 * time.Second)

	if st := b.Snapshot().State; st != resilience.StateHalfOpen {
		t.Fatalf("state after timeout = %s, want half-open", st)
	}
	err := b.Execute(func() error {
		if inner := succeed(b); !errors.Is(inner, resilience.ErrCircuitOpen) {
			t.Errorf("second concurrent call during probe: err = %v, want ErrCircuitOpen", inner)
		}
		return nil
	})

	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if snap := b.Snapshot(); snap.State != resilience.StateClosed || snap.ConsecutiveFailures != 0 {
		t.Fatalf("snapshot = %+v, want closed", snap)
	}
}

func TestBreakerHalfOpenProbeFailureReopensForAFullTimeout(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	b := newBreaker(clock)
	for i := 0; i < 3; i++ {
		_ = fail(b)
	}
	clock.Advance(10 * time.Second)
	_ = fail(b)

	if st := b.Snapshot().State; st != resilience.StateOpen {
		t.Fatalf("state = %s, want open", st)
	}
	clock.Advance(9 * time.Second)
	if err := succeed(b); !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Fatalf("err = %v, want still open", err)
	}
}

func TestBreakerResetClosesAnOpenCircuit(t *testing.T) {
	b := newBreaker(&fakeClock{now: time.Unix(0, 0)})
	for i := 0; i < 3; i++ {
		_ = fail(b)
	}

	b.Reset()

	if snap := b.Snapshot(); snap.State != resilience.StateClosed || snap.ConsecutiveFailures != 0 {
		t.Fatalf("snapshot after Reset = %+v", snap)
	}
	if err := succeed(b); err != nil {
		t.Fatalf("call after Reset: %v", err)
	}
}

func TestNeutralOutcomeNeitherResetsNorCountsFailures(t *testing.T) {
	b := newBreaker(&fakeClock{now: time.Unix(0, 0)})
	_ = fail(b)
	_ = fail(b)

	if err := b.Execute(func() error { return resilience.Neutral(nil) }); err != nil {
		t.Fatalf("a neutral outcome without error returns nil, got %v", err)
	}
	boom := transientErr{true}
	for i := 0; i < 5; i++ {
		if err := b.Execute(func() error { return resilience.Neutral(boom) }); !errors.Is(err, boom) {
			t.Fatalf("neutral error must reach the caller, got %v", err)
		}
	}

	if snap := b.Snapshot(); snap.State != resilience.StateClosed || snap.ConsecutiveFailures != 2 {
		t.Fatalf("snapshot = %+v, want closed with the 2 earlier failures kept", snap)
	}
	_ = fail(b)
	if b.Snapshot().State != resilience.StateOpen {
		t.Fatal("the third real failure must still open the circuit")
	}
}

func TestNeutralOutcomeInHalfOpenKeepsTheCircuitHalfOpenAndFreesTheProbe(t *testing.T) {
	c := &fakeClock{now: time.Unix(0, 0)}
	b := newBreaker(c)
	for i := 0; i < 3; i++ {
		_ = fail(b)
	}
	c.Advance(11 * time.Second)

	if err := b.Execute(func() error { return resilience.Neutral(nil) }); err != nil {
		t.Fatal(err)
	}
	if st := b.Snapshot().State; st != resilience.StateHalfOpen {
		t.Fatalf("state = %s: a run that called no dependency proves nothing", st)
	}
	probed := false
	_ = b.Execute(func() error { probed = true; return nil })
	if !probed {
		t.Fatal("the probe slot must be free for a real call")
	}
	if b.Snapshot().State != resilience.StateClosed {
		t.Fatal("a real successful probe closes the circuit")
	}
}
