package resilience_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/resilience"
)

type transientErr struct{ transient bool }

func (e transientErr) Error() string   { return fmt.Sprintf("status error (transient=%v)", e.transient) }
func (e transientErr) Transient() bool { return e.transient }

type fakeSleeper struct {
	slept []time.Duration
	err   error
}

func (s *fakeSleeper) Sleep(_ context.Context, d time.Duration) error {
	s.slept = append(s.slept, d)
	return s.err
}

func fullJitter() float64 { return 1 }

func newRetrier(max int, s *fakeSleeper, jitter func() float64) *resilience.Retrier {
	return resilience.NewRetrier(resilience.RetryConfig{
		MaxAttempts: max,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    300 * time.Millisecond,
	}, s, jitter)
}

func TestRetrierBacksOffExponentiallyUpToMaxDelayAndStopsAtMaxAttempts(t *testing.T) {
	s := &fakeSleeper{}
	calls := 0

	attempts, err := newRetrier(5, s, fullJitter).Do(context.Background(), func(context.Context) error {
		calls++
		return transientErr{true}
	})

	if attempts != 5 || calls != 5 {
		t.Fatalf("attempts = %d, calls = %d; want 5", attempts, calls)
	}
	var last transientErr
	if !errors.As(err, &last) {
		t.Fatalf("err = %v, want the last transient error", err)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond}
	if fmt.Sprint(s.slept) != fmt.Sprint(want) {
		t.Fatalf("delays = %v, want %v", s.slept, want)
	}
}

func TestRetrierJitterScalesDelayBetweenHalfAndFull(t *testing.T) {
	s := &fakeSleeper{}

	_, _ = newRetrier(2, s, func() float64 { return 0 }).Do(context.Background(), func(context.Context) error {
		return transientErr{true}
	})

	if len(s.slept) != 1 || s.slept[0] != 50*time.Millisecond {
		t.Fatalf("delays = %v, want [50ms]", s.slept)
	}
}

func TestRetrierSucceedsAfterTransientFailures(t *testing.T) {
	s := &fakeSleeper{}
	calls := 0

	attempts, err := newRetrier(5, s, fullJitter).Do(context.Background(), func(context.Context) error {
		calls++
		if calls < 3 {
			return transientErr{true}
		}
		return nil
	})

	if err != nil || attempts != 3 {
		t.Fatalf("attempts = %d, err = %v", attempts, err)
	}
}

func TestRetrierDoesNotRepeatPermanentErrors(t *testing.T) {
	s := &fakeSleeper{}
	calls := 0
	permanent := transientErr{false}

	attempts, err := newRetrier(5, s, fullJitter).Do(context.Background(), func(context.Context) error {
		calls++
		return fmt.Errorf("wrapped: %w", permanent)
	})

	if calls != 1 || attempts != 1 || len(s.slept) != 0 {
		t.Fatalf("calls = %d, attempts = %d, sleeps = %v", calls, attempts, s.slept)
	}
	if !errors.Is(err, permanent) {
		t.Fatalf("err = %v", err)
	}
}

func TestRetrierStopsWhenSleepIsInterrupted(t *testing.T) {
	s := &fakeSleeper{err: context.Canceled}
	calls := 0

	attempts, err := newRetrier(5, s, fullJitter).Do(context.Background(), func(context.Context) error {
		calls++
		return transientErr{true}
	})

	if calls != 1 || attempts != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls = %d, attempts = %d, err = %v", calls, attempts, err)
	}
}

func TestIsTransientClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"marked transient", fmt.Errorf("x: %w", transientErr{true}), true},
		{"marked permanent", transientErr{false}, false},
		{"deadline exceeded", fmt.Errorf("x: %w", context.DeadlineExceeded), true},
		{"canceled", context.Canceled, false},
		{"network error", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"circuit open", resilience.ErrCircuitOpen, false},
		{"unknown", errors.New("boom"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resilience.IsTransient(c.err); got != c.want {
				t.Fatalf("IsTransient(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
