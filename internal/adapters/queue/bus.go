package queue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

type Config struct {
	Stream   string
	Group    string
	DLQ      string
	Consumer string
	// Block is how long Receive waits for a new message; defaults to 1s.
	Block time.Duration
	// ClaimIdle is how long a delivered message may stay unacked before any
	// consumer of the group can take it over. Zero disables takeover.
	ClaimIdle time.Duration
	Now       func() time.Time
}

// Bus publishes to a Redis stream and consumes it through a consumer group.
type Bus struct {
	client redis.Cmdable
	cfg    Config
}

// New creates the stream and consumer group when they do not exist yet.
func New(ctx context.Context, client redis.Cmdable, cfg Config) (*Bus, error) {
	if cfg.Block <= 0 {
		cfg.Block = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	err := client.XGroupCreateMkStream(ctx, cfg.Stream, cfg.Group, "0").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return nil, fmt.Errorf("create consumer group: %w", err)
	}
	return &Bus{client: client, cfg: cfg}, nil
}

func (b *Bus) Publish(ctx context.Context, event domain.Event) error {
	err := b.client.XAdd(ctx, &redis.XAddArgs{
		Stream: b.cfg.Stream,
		Values: map[string]any{"type": string(event.Type), "order_id": event.OrderID},
	}).Err()
	if err != nil {
		return fmt.Errorf("publish to stream %s: %w", b.cfg.Stream, err)
	}
	return nil
}

func (b *Bus) Receive(ctx context.Context) (ports.Delivery, error) {
	for {
		msg, err := b.next(ctx)
		if err != nil {
			return ports.Delivery{}, err
		}
		event, err := decodeEvent(msg.Values)
		if err != nil {
			reason := "malformed message: " + err.Error()
			if rejectErr := b.Reject(ctx, ports.Delivery{ID: msg.ID}, reason, 0); rejectErr != nil {
				return ports.Delivery{}, rejectErr
			}
			continue
		}
		return ports.Delivery{ID: msg.ID, Event: event}, nil
	}
}

func (b *Bus) Ack(ctx context.Context, deliveryID string) error {
	pipe := b.client.TxPipeline()
	pipe.XAck(ctx, b.cfg.Stream, b.cfg.Group, deliveryID)
	pipe.XDel(ctx, b.cfg.Stream, deliveryID)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("ack %s: %w", deliveryID, err)
	}
	return nil
}

func (b *Bus) Reject(ctx context.Context, delivery ports.Delivery, reason string, attempts int) error {
	pipe := b.client.TxPipeline()
	pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: b.cfg.DLQ,
		Values: map[string]any{
			"type":      string(delivery.Event.Type),
			"order_id":  delivery.Event.OrderID,
			"reason":    reason,
			"attempts":  attempts,
			"failed_at": b.cfg.Now().UTC().Format(time.RFC3339Nano),
			"source_id": delivery.ID,
		},
	})
	pipe.XAck(ctx, b.cfg.Stream, b.cfg.Group, delivery.ID)
	pipe.XDel(ctx, b.cfg.Stream, delivery.ID)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("reject %s to dead-letter queue: %w", delivery.ID, err)
	}
	return nil
}

func (b *Bus) DeadLetters(ctx context.Context) ([]ports.DeadLetter, error) {
	msgs, err := b.client.XRange(ctx, b.cfg.DLQ, "-", "+").Result()
	if err != nil {
		return nil, fmt.Errorf("read dead-letter queue: %w", err)
	}
	letters := make([]ports.DeadLetter, 0, len(msgs))
	for _, m := range msgs {
		event, _ := decodeEvent(m.Values)
		attempts, _ := strconv.Atoi(str(m.Values["attempts"]))
		failedAt, _ := time.Parse(time.RFC3339Nano, str(m.Values["failed_at"]))
		letters = append(letters, ports.DeadLetter{
			ID: m.ID, Event: event, Reason: str(m.Values["reason"]), Attempts: attempts, FailedAt: failedAt,
		})
	}
	return letters, nil
}

func (b *Bus) Discard(ctx context.Context, deadLetterID string) error {
	if err := b.client.XDel(ctx, b.cfg.DLQ, deadLetterID).Err(); err != nil {
		return fmt.Errorf("discard dead letter %s: %w", deadLetterID, err)
	}
	return nil
}

// next prefers a message abandoned by another consumer over a new one.
func (b *Bus) next(ctx context.Context) (redis.XMessage, error) {
	if b.cfg.ClaimIdle > 0 {
		claimed, _, err := b.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream: b.cfg.Stream, Group: b.cfg.Group, Consumer: b.cfg.Consumer,
			MinIdle: b.cfg.ClaimIdle, Start: "0-0", Count: 1,
		}).Result()
		if err != nil {
			return redis.XMessage{}, fmt.Errorf("claim abandoned messages: %w", err)
		}
		if len(claimed) > 0 {
			return claimed[0], nil
		}
	}
	streams, err := b.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: b.cfg.Group, Consumer: b.cfg.Consumer,
		Streams: []string{b.cfg.Stream, ">"}, Count: 1, Block: b.cfg.Block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return redis.XMessage{}, ports.ErrNoDelivery
	}
	if err != nil {
		return redis.XMessage{}, fmt.Errorf("read stream %s: %w", b.cfg.Stream, err)
	}
	return streams[0].Messages[0], nil
}

func decodeEvent(values map[string]any) (domain.Event, error) {
	typ, id := str(values["type"]), str(values["order_id"])
	if typ == "" || id == "" {
		return domain.Event{}, errors.New("missing type or order_id")
	}
	return domain.Event{Type: domain.EventType(typ), OrderID: id}, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
