package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/queue"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

var failedAt = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func newClient(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client, mr
}

func newBus(t *testing.T, client *redis.Client, consumer string) *queue.Bus {
	t.Helper()
	bus, err := queue.New(context.Background(), client, queue.Config{
		Stream:    "orders",
		Group:     "workers",
		DLQ:       "orders-dlq",
		Consumer:  consumer,
		Block:     5 * time.Millisecond,
		ClaimIdle: time.Minute,
		Now:       func() time.Time { return failedAt },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return bus
}

var authorized = domain.Event{Type: domain.EventOrderAuthorized, OrderID: "ord-1"}

func pending(t *testing.T, client *redis.Client) int64 {
	t.Helper()
	p, err := client.XPending(context.Background(), "orders", "workers").Result()
	if err != nil {
		t.Fatal(err)
	}
	return p.Count
}

func TestPublishThenReceiveDeliversTheEventAndAckClearsIt(t *testing.T) {
	ctx := context.Background()
	client, _ := newClient(t)
	bus := newBus(t, client, "w1")
	if err := bus.Publish(ctx, authorized); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	d, err := bus.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if d.Event != authorized || d.ID == "" {
		t.Fatalf("delivery = %+v", d)
	}
	if pending(t, client) != 1 {
		t.Fatal("a received message stays pending until acked")
	}
	if err := bus.Ack(ctx, d.ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if pending(t, client) != 0 {
		t.Fatal("acked message must not stay pending")
	}
}

func TestReceiveWithoutMessagesReturnsErrNoDelivery(t *testing.T) {
	client, _ := newClient(t)
	if _, err := newBus(t, client, "w1").Receive(context.Background()); !errors.Is(err, ports.ErrNoDelivery) {
		t.Fatalf("err = %v", err)
	}
}

func TestConsumersOfTheSameGroupShareTheStream(t *testing.T) {
	ctx := context.Background()
	client, _ := newClient(t)
	w1, w2 := newBus(t, client, "w1"), newBus(t, client, "w2")
	_ = w1.Publish(ctx, domain.Event{Type: domain.EventOrderAuthorized, OrderID: "a"})
	_ = w1.Publish(ctx, domain.Event{Type: domain.EventOrderAuthorized, OrderID: "b"})

	d1, err1 := w1.Receive(ctx)
	d2, err2 := w2.Receive(ctx)

	if err1 != nil || err2 != nil || d1.Event.OrderID == d2.Event.OrderID {
		t.Fatalf("d1 = %+v (%v), d2 = %+v (%v)", d1, err1, d2, err2)
	}
}

func TestUnackedMessageIsRedeliveredToAnotherConsumerAfterTheIdleTime(t *testing.T) {
	ctx := context.Background()
	client, mr := newClient(t)
	crashed, survivor := newBus(t, client, "w1"), newBus(t, client, "w2")
	_ = crashed.Publish(ctx, authorized)
	first, _ := crashed.Receive(ctx)

	mr.SetTime(time.Now().Add(2 * time.Minute))
	again, err := survivor.Receive(ctx)

	if err != nil || again.ID != first.ID || again.Event != authorized {
		t.Fatalf("redelivery = %+v, err = %v; want %+v", again, err, first)
	}
}

func TestRejectMovesTheMessageToTheDLQWithReasonAndAttempts(t *testing.T) {
	ctx := context.Background()
	client, _ := newClient(t)
	bus := newBus(t, client, "w1")
	_ = bus.Publish(ctx, authorized)
	d, _ := bus.Receive(ctx)

	if err := bus.Reject(ctx, d, "erp unavailable", 5); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	if pending(t, client) != 0 {
		t.Fatal("rejected message must leave the pending list")
	}
	letters, err := bus.DeadLetters(ctx)
	if err != nil || len(letters) != 1 {
		t.Fatalf("letters = %+v, err = %v", letters, err)
	}
	got := letters[0]
	if got.Event != authorized || got.Reason != "erp unavailable" || got.Attempts != 5 || !got.FailedAt.Equal(failedAt) || got.ID == "" {
		t.Fatalf("dead letter = %+v", got)
	}
}

func TestDiscardRemovesADeadLetter(t *testing.T) {
	ctx := context.Background()
	client, _ := newClient(t)
	bus := newBus(t, client, "w1")
	_ = bus.Publish(ctx, authorized)
	d, _ := bus.Receive(ctx)
	_ = bus.Reject(ctx, d, "x", 1)
	letters, _ := bus.DeadLetters(ctx)

	if err := bus.Discard(ctx, letters[0].ID); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	if after, _ := bus.DeadLetters(ctx); len(after) != 0 {
		t.Fatalf("dead letters = %+v", after)
	}
}

func TestMalformedMessageGoesStraightToTheDLQInsteadOfBlockingTheQueue(t *testing.T) {
	ctx := context.Background()
	client, _ := newClient(t)
	bus := newBus(t, client, "w1")
	client.XAdd(ctx, &redis.XAddArgs{Stream: "orders", Values: map[string]any{"junk": "1"}})
	_ = bus.Publish(ctx, authorized)

	d, err := bus.Receive(ctx)

	if err != nil || d.Event != authorized {
		t.Fatalf("delivery = %+v, err = %v; want the valid message after skipping the malformed one", d, err)
	}
	letters, _ := bus.DeadLetters(ctx)
	if len(letters) != 1 || letters[0].Attempts != 0 || letters[0].Reason == "" {
		t.Fatalf("dead letters = %+v", letters)
	}
}

func TestRestartedConsumerReceivesItsOwnPendingMessagesFirst(t *testing.T) {
	ctx := context.Background()
	client, _ := newClient(t)
	before := newBus(t, client, "w1")
	_ = before.Publish(ctx, authorized)
	first, _ := before.Receive(ctx) // interrupted by a shutdown: neither acked nor rejected

	after := newBus(t, client, "w1")
	again, err := after.Receive(ctx)

	if err != nil || again.ID != first.ID {
		t.Fatalf("after restart: delivery = %+v, err = %v; want %+v", again, err, first)
	}
	_ = after.Ack(ctx, again.ID)
	if _, err := after.Receive(ctx); !errors.Is(err, ports.ErrNoDelivery) {
		t.Fatalf("second receive err = %v, want ErrNoDelivery", err)
	}
}

func TestNewRequiresAPositiveClaimIdleSoAbandonedMessagesAreNeverStuck(t *testing.T) {
	client, _ := newClient(t)
	_, err := queue.New(context.Background(), client, queue.Config{Stream: "orders", Group: "workers", DLQ: "dlq", Consumer: "w1"})
	if err == nil {
		t.Fatal("ClaimIdle = 0 must be rejected")
	}
}

func TestPublishCapsTheStreamLengthAndRejectCapsTheDLQ(t *testing.T) {
	ctx := context.Background()
	client, _ := newClient(t)
	bus, err := queue.New(ctx, client, queue.Config{
		Stream: "orders", Group: "workers", DLQ: "orders-dlq", Consumer: "w1",
		Block: 5 * time.Millisecond, ClaimIdle: time.Minute, MaxLen: 5, DLQMaxLen: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		if err := bus.Publish(ctx, authorized); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		d, err := bus.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Reject(ctx, d, "x", 1); err != nil {
			t.Fatal(err)
		}
	}

	if n := client.XLen(ctx, "orders").Val(); n > 5 {
		t.Fatalf("stream length = %d, want at most 5", n)
	}
	if n := client.XLen(ctx, "orders-dlq").Val(); n > 3 {
		t.Fatalf("DLQ length = %d, want at most 3", n)
	}
}

func TestResetEmptiesStreamAndDLQAndTheBusKeepsWorking(t *testing.T) {
	ctx := context.Background()
	client, _ := newClient(t)
	bus := newBus(t, client, "w1")
	_ = bus.Publish(ctx, authorized)
	d, _ := bus.Receive(ctx)
	_ = bus.Reject(ctx, d, "x", 1)
	_ = bus.Publish(ctx, authorized)

	if err := bus.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if letters, _ := bus.DeadLetters(ctx); len(letters) != 0 {
		t.Fatalf("dead letters after Reset = %+v", letters)
	}
	if _, err := bus.Receive(ctx); !errors.Is(err, ports.ErrNoDelivery) {
		t.Fatalf("Receive after Reset = %v, want ErrNoDelivery", err)
	}
	_ = bus.Publish(ctx, authorized)
	if got, err := bus.Receive(ctx); err != nil || got.Event != authorized {
		t.Fatalf("Receive after republish = %+v, %v", got, err)
	}
}
