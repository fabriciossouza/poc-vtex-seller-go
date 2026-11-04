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
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

// uniqueConstraint is SQLITE_CONSTRAINT_UNIQUE.
const uniqueConstraint = 2067

// primaryKeyConstraint is SQLITE_CONSTRAINT_PRIMARYKEY.
const primaryKeyConstraint = 1555

const schema = `
CREATE TABLE IF NOT EXISTS orders (
	id TEXT PRIMARY KEY,
	idempotency_key TEXT NOT NULL UNIQUE,
	items TEXT NOT NULL,
	status TEXT NOT NULL,
	tracking_code TEXT NOT NULL,
	created_at TEXT NOT NULL,
	marketplace_endpoint TEXT NOT NULL,
	version INTEGER NOT NULL DEFAULT 0
)`

const columns = "id, idempotency_key, items, status, tracking_code, created_at, marketplace_endpoint, version"

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

// Reset deletes every order; it exists for the periodic demo cleanup.
func (r *Repo) Reset(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM orders`); err != nil {
		return fmt.Errorf("reset orders: %w", err)
	}
	return nil
}

// ErrIDCollision means a new order carries the id of a stored order that has
// another idempotency key; the caller should retry with a fresh id.
var ErrIDCollision = errors.New("order id already belongs to another order")

// Save inserts a brand-new order (version 0, status received) and updates any
// other one if its version still matches the stored one (optimistic locking).
// An update that matches no row returns ports.ErrVersionConflict when the
// order exists and domain.ErrOrderNotFound when it does not, so a reset can
// never be undone by a late update.
func (r *Repo) Save(ctx context.Context, order *domain.Order) error {
	if order.Version == 0 && order.Status == domain.StatusReceived {
		return r.insert(ctx, order)
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE orders SET status = ?, tracking_code = ?, version = version + 1 WHERE id = ? AND version = ?`,
		string(order.Status), order.TrackingCode, order.ID, order.Version)
	if err != nil {
		return fmt.Errorf("update order %s: %w", order.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		order.Version++
		return nil
	}
	var exists int
	switch err := r.db.QueryRowContext(ctx, `SELECT 1 FROM orders WHERE id = ?`, order.ID).Scan(&exists); {
	case err == nil:
		return ports.ErrVersionConflict
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("update order %s: %w", order.ID, domain.ErrOrderNotFound)
	default:
		return fmt.Errorf("check order %s: %w", order.ID, err)
	}
}

func (r *Repo) insert(ctx context.Context, order *domain.Order) error {
	items, err := json.Marshal(order.Items)
	if err != nil {
		return fmt.Errorf("encode items: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO orders (`+columns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		order.ID, order.IdempotencyKey, string(items), string(order.Status), order.TrackingCode,
		order.CreatedAt.UTC().Format(time.RFC3339Nano), order.MarketplaceEndpoint, order.Version)
	if err == nil {
		return nil
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || (sqliteErr.Code() != uniqueConstraint && sqliteErr.Code() != primaryKeyConstraint) {
		return fmt.Errorf("save order %s: %w", order.ID, err)
	}
	existing, findErr := r.FindByIdempotencyKey(ctx, order.IdempotencyKey)
	if errors.Is(findErr, domain.ErrOrderNotFound) {
		return fmt.Errorf("save order %s: %w", order.ID, ErrIDCollision)
	}
	if findErr != nil {
		return fmt.Errorf("load order after idempotency conflict: %w", findErr)
	}
	if existing.ID != order.ID && sqliteErr.Code() == primaryKeyConstraint {
		return fmt.Errorf("save order %s: %w", order.ID, ErrIDCollision)
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
	// column is always a constant chosen by Get or FindByIdempotencyKey; value is bound.
	row := r.db.QueryRowContext(ctx, "SELECT "+columns+" FROM orders WHERE "+column+" = ?", value) // #nosec G202
	var (
		o                      domain.Order
		items, status, created string
	)
	err := row.Scan(&o.ID, &o.IdempotencyKey, &items, &status, &o.TrackingCode, &created, &o.MarketplaceEndpoint, &o.Version)
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
