package resilience

import (
	"errors"
	"sync"
	"time"
)

type State string

const (
	StateClosed   State = "closed"
	StateOpen     State = "open"
	StateHalfOpen State = "half-open"
)

type BreakerConfig struct {
	// FailureThreshold is the number of consecutive transient failures that opens the circuit.
	FailureThreshold int
	// OpenTimeout is how long the circuit rejects calls before allowing a probe.
	OpenTimeout time.Duration
}

// Snapshot is the observable state of a Breaker, meant for dashboards.
type Snapshot struct {
	State               State
	ConsecutiveFailures int
	OpenedAt            time.Time
}

// Breaker counts only transient failures: a permanent error (such as a 4xx)
// shows the dependency answered, so it counts as a healthy call.
type Breaker struct {
	cfg BreakerConfig
	now func() time.Time

	mu       sync.Mutex
	state    State
	failures int
	openedAt time.Time
	probing  bool
}

func NewBreaker(cfg BreakerConfig, now func() time.Time) *Breaker {
	return &Breaker{cfg: cfg, now: now, state: StateClosed}
}

func (b *Breaker) Execute(fn func() error) error {
	if err := b.admit(); err != nil {
		return err
	}
	err := fn()
	var n *neutral
	if errors.As(err, &n) {
		b.release()
		return n.err
	}
	b.record(IsTransient(err))
	return err
}

// release frees the probe slot without recording an outcome.
func (b *Breaker) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}

func (b *Breaker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.promote()
	return Snapshot{State: b.state, ConsecutiveFailures: b.failures, OpenedAt: b.openedAt}
}

// Reset closes the circuit and forgets past failures.
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state, b.failures, b.probing, b.openedAt = StateClosed, 0, false, time.Time{}
}

// promote moves open to half-open once the timeout has elapsed.
func (b *Breaker) promote() {
	if b.state == StateOpen && !b.now().Before(b.openedAt.Add(b.cfg.OpenTimeout)) {
		b.state = StateHalfOpen
		b.probing = false
	}
}

func (b *Breaker) admit() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.promote()
	switch b.state {
	case StateOpen:
		return ErrCircuitOpen
	case StateHalfOpen:
		if b.probing {
			return ErrCircuitOpen
		}
		b.probing = true
	}
	return nil
}

func (b *Breaker) record(failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !failed {
		b.state, b.failures, b.probing = StateClosed, 0, false
		return
	}
	b.failures++
	if b.state == StateHalfOpen || b.failures >= b.cfg.FailureThreshold {
		b.state, b.openedAt, b.probing = StateOpen, b.now(), false
	}
}
