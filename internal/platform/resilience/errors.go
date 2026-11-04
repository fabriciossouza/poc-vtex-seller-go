package resilience

import (
	"context"
	"errors"
	"net"
)

var ErrCircuitOpen = errors.New("circuit breaker is open")

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
