// Package worker consumes OrderAuthorized events and runs the dispatch use
// case behind retry and a circuit breaker, parking failures in a DLQ.
package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/resilience"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type Dispatcher interface {
	Execute(ctx context.Context, orderID string) error
}

type ReplayResult struct {
	Replayed int
	// Failed letters stay in the DLQ for another attempt.
	Failed int
	// Discarded letters can never succeed (malformed, unknown or terminal order).
	Discarded int
}

type Worker struct {
	source   ports.EventConsumer
	dispatch Dispatcher
	retrier  *resilience.Retrier
	breaker  *resilience.Breaker

	replayMu sync.Mutex
}

func New(source ports.EventConsumer, dispatch Dispatcher, retrier *resilience.Retrier, breaker *resilience.Breaker) *Worker {
	return &Worker{source: source, dispatch: dispatch, retrier: retrier, breaker: breaker}
}

// Run processes deliveries until ctx is cancelled or the queue itself fails.
func (w *Worker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := w.ProcessNext(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ProcessNext handles at most one delivery. It returns an error only when the
// queue misbehaves; a failed dispatch is parked in the DLQ and is not an error.
func (w *Worker) ProcessNext(ctx context.Context) error {
	d, err := w.source.Receive(ctx)
	if errors.Is(err, ports.ErrNoDelivery) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("receive: %w", err)
	}
	if d.Event.Type != domain.EventOrderAuthorized {
		return w.source.Reject(ctx, d, fmt.Sprintf("unsupported event type %q", d.Event.Type), 0)
	}

	attempts, err := w.run(ctx, d.Event.OrderID)
	if err != nil && ctx.Err() != nil {
		// Shutting down: leave the delivery pending so it is redelivered.
		return ctx.Err()
	}
	if err != nil {
		return w.source.Reject(ctx, d, reasonOf(err), attempts)
	}
	return w.source.Ack(ctx, d.ID)
}

// ReplayDLQ runs every dead letter through the same retry and breaker.
// Dispatching resumes from the order's status, so steps that already
// succeeded are not sent again. Replays are serialized: the DLQ is listed
// under the lock, so a concurrent caller sees what the first one discarded.
func (w *Worker) ReplayDLQ(ctx context.Context) (ReplayResult, error) {
	w.replayMu.Lock()
	defer w.replayMu.Unlock()

	letters, err := w.source.DeadLetters(ctx)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("list dead letters: %w", err)
	}
	var res ReplayResult
	for _, l := range letters {
		outcome, err := w.replayOne(ctx, l)
		if err != nil {
			return res, err
		}
		switch outcome {
		case replayed:
			res.Replayed++
		case discarded:
			res.Discarded++
		default:
			res.Failed++
		}
	}
	return res, nil
}

type replayOutcome int

const (
	kept replayOutcome = iota
	replayed
	discarded
)

func (w *Worker) replayOne(ctx context.Context, l ports.DeadLetter) (replayOutcome, error) {
	outcome := replayed
	if l.Event.Type != domain.EventOrderAuthorized || l.Event.OrderID == "" {
		outcome = discarded
	} else if _, err := w.run(ctx, l.Event.OrderID); err != nil {
		switch {
		case ctx.Err() != nil:
			return kept, ctx.Err()
		case errors.Is(err, domain.ErrOrderNotFound), errors.Is(err, domain.ErrInvalidTransition):
			outcome = discarded
		default:
			return kept, nil
		}
	}
	if err := w.source.Discard(ctx, l.ID); err != nil {
		return kept, err
	}
	return outcome, nil
}

type runError struct {
	err        error
	lastFailed error
}

func (e *runError) Error() string { return reasonOf(e) }
func (e *runError) Unwrap() error { return e.err }

func (w *Worker) run(ctx context.Context, orderID string) (int, error) {
	var lastFailure error
	attempts, err := w.retrier.Do(ctx, func(ctx context.Context) error {
		return w.breaker.Execute(func() error {
			err := w.dispatch.Execute(ctx, orderID)
			if err != nil {
				lastFailure = err
			}
			return err
		})
	})
	if err != nil {
		return attempts, &runError{err: err, lastFailed: lastFailure}
	}
	return attempts, nil
}

// reasonOf keeps the dependency failure visible when the breaker rejected the
// final attempt, since "circuit open" alone says nothing about the cause.
func reasonOf(err error) string {
	var re *runError
	if errors.As(err, &re) {
		if errors.Is(re.err, resilience.ErrCircuitOpen) && re.lastFailed != nil {
			return fmt.Sprintf("%v (last failure: %v)", re.err, re.lastFailed)
		}
		return re.err.Error()
	}
	return err.Error()
}
