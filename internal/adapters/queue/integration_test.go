//go:build integration

package queue_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/queue"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

// These tests run against a real Redis (REDIS_ADDR), because miniredis does
// not model every XACK/XPENDING detail the production path relies on.

type realRedis struct {
	client *redis.Client
	prefix string
}

func newRealRedis(t *testing.T) *realRedis {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR is not set")
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	r := &realRedis{client: redis.NewClient(&redis.Options{Addr: addr}), prefix: "it-" + hex.EncodeToString(suffix)}
	t.Cleanup(func() {
		r.client.Del(context.Background(), r.prefix+".events", r.prefix+".dlq")
		_ = r.client.Close()
	})
	return r
}

func (r *realRedis) bus(t *testing.T, consumer string) *queue.Bus {
	t.Helper()
	bus, err := queue.New(context.Background(), r.client, queue.Config{
		Stream: r.prefix + ".events", Group: "workers", DLQ: r.prefix + ".dlq", Consumer: consumer,
		Block: 50 * time.Millisecond, ClaimIdle: time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return bus
}

func (r *realRedis) pendingCount(t *testing.T) int64 {
	t.Helper()
	p, err := r.client.XPending(context.Background(), r.prefix+".events", "workers").Result()
	if err != nil {
		t.Fatal(err)
	}
	return p.Count
}

func TestRealRedisRejectAcksTheMessageAndMovesItToTheDLQ(t *testing.T) {
	ctx := context.Background()
	r := newRealRedis(t)
	bus := r.bus(t, "w1")
	_ = bus.Publish(ctx, authorized)
	d, err := bus.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.pendingCount(t) != 1 {
		t.Fatalf("pending before Reject = %d, want 1", r.pendingCount(t))
	}

	if err := bus.Reject(ctx, d, "erp unavailable", 4); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	if r.pendingCount(t) != 0 {
		t.Fatalf("pending after Reject = %d: XACK did not happen", r.pendingCount(t))
	}
	if n := r.client.XLen(ctx, r.prefix+".events").Val(); n != 0 {
		t.Fatalf("stream length = %d, want 0", n)
	}
	letters, err := bus.DeadLetters(ctx)
	if err != nil || len(letters) != 1 || letters[0].Reason != "erp unavailable" || letters[0].Attempts != 4 {
		t.Fatalf("letters = %+v, err = %v", letters, err)
	}
}

func TestRealRedisAckClearsThePendingEntry(t *testing.T) {
	ctx := context.Background()
	r := newRealRedis(t)
	bus := r.bus(t, "w1")
	_ = bus.Publish(ctx, authorized)
	d, _ := bus.Receive(ctx)

	if err := bus.Ack(ctx, d.ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	if r.pendingCount(t) != 0 {
		t.Fatalf("pending after Ack = %d", r.pendingCount(t))
	}
	if _, err := bus.Receive(ctx); !errors.Is(err, ports.ErrNoDelivery) {
		t.Fatalf("Receive after Ack = %v, want ErrNoDelivery", err)
	}
}

func TestRealRedisPendingMessageIsRedeliveredWhenTheConsumerRestarts(t *testing.T) {
	ctx := context.Background()
	r := newRealRedis(t)
	before := r.bus(t, "w1")
	_ = before.Publish(ctx, authorized)
	first, _ := before.Receive(ctx) // crash: never acked

	restarted := r.bus(t, "w1")
	again, err := restarted.Receive(ctx)

	if err != nil || again.ID != first.ID || again.Event != authorized {
		t.Fatalf("after restart: %+v, %v; want %+v", again, err, first)
	}
}

func TestRealRedisResetEmptiesBothStreamsAndTheGroupWorksAgain(t *testing.T) {
	ctx := context.Background()
	r := newRealRedis(t)
	bus := r.bus(t, "w1")
	_ = bus.Publish(ctx, authorized)
	d, _ := bus.Receive(ctx)
	_ = bus.Reject(ctx, d, "x", 1)

	if err := bus.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if letters, _ := bus.DeadLetters(ctx); len(letters) != 0 {
		t.Fatalf("letters = %+v", letters)
	}
	_ = bus.Publish(ctx, authorized)
	if got, err := bus.Receive(ctx); err != nil || got.Event != authorized {
		t.Fatalf("Receive after Reset = %+v, %v", got, err)
	}
}
