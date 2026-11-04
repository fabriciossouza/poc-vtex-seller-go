package resilience

import (
	"context"
	"math/rand/v2"
	"time"
)

type Sleeper interface {
	Sleep(ctx context.Context, d time.Duration) error
}

type SystemSleeper struct{}

func (SystemSleeper) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

type Retrier struct {
	cfg     RetryConfig
	sleeper Sleeper
	jitter  func() float64
}

// NewRetrier takes a jitter source returning values in [0, 1); nil uses
// math/rand. Each wait is the exponential delay scaled into [50%, 100%].
func NewRetrier(cfg RetryConfig, sleeper Sleeper, jitter func() float64) *Retrier {
	if jitter == nil {
		jitter = rand.Float64
	}
	return &Retrier{cfg: cfg, sleeper: sleeper, jitter: jitter}
}

// Do runs fn until it succeeds, fails with a non-transient error or runs out
// of attempts. It returns how many times fn was called and the last error.
func (r *Retrier) Do(ctx context.Context, fn func(context.Context) error) (int, error) {
	var err error
	for attempt := 1; ; attempt++ {
		err = fn(ctx)
		if err == nil {
			return attempt, nil
		}
		if !IsTransient(err) || attempt >= r.cfg.MaxAttempts {
			return attempt, err
		}
		if sleepErr := r.sleeper.Sleep(ctx, r.delay(attempt)); sleepErr != nil {
			return attempt, sleepErr
		}
	}
}

func (r *Retrier) delay(failedAttempt int) time.Duration {
	d := r.cfg.BaseDelay << (failedAttempt - 1)
	if d <= 0 || d > r.cfg.MaxDelay {
		d = r.cfg.MaxDelay
	}
	half := d / 2
	return half + time.Duration(float64(half)*r.jitter())
}
