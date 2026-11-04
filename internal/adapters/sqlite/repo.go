package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
)

// uniqueConstraint is SQLITE_CONSTRAINT_UNIQUE.
const uniqueConstraint = 2067

const schema = `
CREATE TABLE IF NOT EXISTS orders (
	id TEXT PRIMARY KEY,
	idempotency_key TEXT NOT NULL UNIQUE,
	items TEXT NOT NULL,
	status TEXT NOT NULL,
	tracking_code TEXT NOT NULL,
	created_at TEXT NOT NULL,
	marketplace_endpoint TEXT NOT NULL
)`

const columns = "id, idempotency_key, items, status, tracking_code, created_at, marketplace_endpoint"

type Repo struct {
	db *sql.DB
}

// Open uses a single connection: SQLite allows one writer anyway, and it
// avoids "database is locked" under concurrent requests.
func Open(path string) (*Repo, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Repo{db: db}, nil
}

func (r *Repo) Close() error { return r.db.Close() }

func (r *Repo) Save(ctx context.Context, order *domain.Order) error {
	items, err := json.Marshal(order.Items)
	if err != nil {
		return fmt.Errorf("encode items: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO orders (`+columns+`) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET status = excluded.status, tracking_code = excluded.tracking_code`,
		order.ID, order.IdempotencyKey, string(items), string(order.Status), order.TrackingCode,
		order.CreatedAt.UTC().Format(time.RFC3339Nano), order.MarketplaceEndpoint)
	if err == nil {
		return nil
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code() != uniqueConstraint {
		return fmt.Errorf("save order %s: %w", order.ID, err)
	}
	existing, findErr := r.FindByIdempotencyKey(ctx, order.IdempotencyKey)
	if findErr != nil {
		return fmt.Errorf("load order after idempotency conflict: %w", findErr)
	}
	*order = *existing
	return nil
}

func (r *Repo) Get(ctx context.Context, id string) (*domain.Order, error) {
	return r.queryOne(ctx, "id", id)
}

func (r *Repo) FindByIdempotencyKey(ctx context.Context, key string) (*domain.Order, error) {
	return r.queryOne(ctx, "idempotency_key", key)
}

func (r *Repo) queryOne(ctx context.Context, column, value string) (*domain.Order, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+columns+" FROM orders WHERE "+column+" = ?", value)
	var (
		o                      domain.Order
		items, status, created string
	)
	err := row.Scan(&o.ID, &o.IdempotencyKey, &items, &status, &o.TrackingCode, &created, &o.MarketplaceEndpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrOrderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query order by %s: %w", column, err)
	}
	if err := json.Unmarshal([]byte(items), &o.Items); err != nil {
		return nil, fmt.Errorf("decode items of %s: %w", o.ID, err)
	}
	if o.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, fmt.Errorf("decode created_at of %s: %w", o.ID, err)
	}
	o.Status = domain.OrderStatus(status)
	return &o, nil
}
