package resilience

import (
	"context"
	"errors"
	"net"
)

var ErrCircuitOpen = errors.New("circuit breaker is open")

type neutral struct{ err error }

func (n *neutral) Error() string {
	if n.err == nil {
		return "neutral outcome"
	}
	return n.err.Error()
}

func (n *neutral) Unwrap() error { return n.err }

// Neutral marks the outcome of a call as saying nothing about the health of
// the dependency: the breaker neither counts it as a failure nor closes on it,
// and hands err (possibly nil) back to the caller.
func Neutral(err error) error { return &neutral{err: err} }

// IsTransient reports whether retrying the same call may succeed. Errors can
// opt in or out by implementing Transient() bool; anything unknown is treated
// as permanent so a bug is never retried silently.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, ErrCircuitOpen) {
		return false
	}
	var marked interface{ Transient() bool }
	if errors.As(err, &marked) {
		return marked.Transient()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
