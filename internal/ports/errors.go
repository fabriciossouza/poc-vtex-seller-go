package ports

import "fmt"

// StatusError is returned by HTTP adapters when a dependency answers with a
// non-success status. Only server errors are transient: a 4xx means the
// request itself is wrong and repeating it cannot help.
type StatusError struct {
	Service    string
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s responded with status %d", e.Service, e.StatusCode)
}

func (e *StatusError) Transient() bool { return e.StatusCode >= 500 }

type versionConflict struct{}

func (versionConflict) Error() string { return "order was modified concurrently" }

// Transient: the caller can reload the order and try again.
func (versionConflict) Transient() bool { return true }

// ErrVersionConflict is returned by OrderRepo.Save when the order changed
// since it was read.
var ErrVersionConflict error = versionConflict{}
