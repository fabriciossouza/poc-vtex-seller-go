package httpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	httpadapter "github.com/fabriciossouza/poc-vtex-seller-go/internal/adapters/http"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/app"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/domain"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

var fixedNow = time.Date(2026, 11, 4, 18, 52, 0, 0, time.UTC)

type fixedClock struct{}

func (fixedClock) Now() time.Time { return fixedNow }

type fakeCatalog struct{}

func (fakeCatalog) UnitPriceCents(context.Context, string) (int64, error) { return 2499, nil }

type fakeStock struct{ levels map[string]int }

func (f fakeStock) Available(_ context.Context, sku string) (int, error) { return f.levels[sku], nil }

// soldOutReserver accepts every reservation except for the sku "sold-out".
type soldOutReserver struct{}

func (soldOutReserver) Reserve(_ context.Context, sku string, _ int) error {
	if sku == "sold-out" {
		return ports.ErrInsufficientStock
	}
	return nil
}

func (soldOutReserver) Release(context.Context, string, int) error { return nil }

type fakeTMS struct{}

func (fakeTMS) QuoteShipping(context.Context, []domain.Item) (domain.ShippingSLA, error) {
	return domain.ShippingSLA{PriceCents: 1099, Estimate: 168 * time.Hour}, nil
}

func (fakeTMS) CreateShipment(context.Context, domain.Order) (string, error) { return "TRK", nil }

type memRepo struct {
	mu   sync.Mutex
	byID map[string]domain.Order
}

func (r *memRepo) Save(_ context.Context, o *domain.Order) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[o.ID] = *o
	return nil
}

func (r *memRepo) Get(_ context.Context, id string) (*domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.byID[id]
	if !ok {
		return nil, domain.ErrOrderNotFound
	}
	return &o, nil
}

func (r *memRepo) FindByIdempotencyKey(_ context.Context, key string) (*domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.byID {
		if o.IdempotencyKey == key {
			return &o, nil
		}
	}
	return nil, domain.ErrOrderNotFound
}

type fakeBus struct{ events []domain.Event }

func (b *fakeBus) Publish(_ context.Context, e domain.Event) error {
	b.events = append(b.events, e)
	return nil
}

type env struct {
	srv  *httptest.Server
	repo *memRepo
	bus  *fakeBus
}

func newEnv(t *testing.T, stock map[string]int) *env {
	t.Helper()
	repo, bus := &memRepo{byID: map[string]domain.Order{}}, &fakeBus{}
	ids := 0
	handler := httpadapter.NewHandler(httpadapter.Deps{
		Simulate: app.NewSimulateCart(fakeCatalog{}, fakeStock{stock}, fakeTMS{}),
		Place:    app.NewPlaceOrder(repo, fixedClock{}, soldOutReserver{}),
		Fulfill:  app.NewFulfillOrder(repo, bus),
		Cancel:   app.NewCancelOrder(repo, soldOutReserver{}),
		Clock:    fixedClock{},
		ValidateEndpoint: func(endpoint string) error {
			if endpoint == "https://marketplace-exemplo.myvtex.com/" {
				return nil
			}
			return errors.New("endpoint not allowed")
		},
		NewID: func() string { ids++; return "95931109" + strconv.Itoa(ids) },
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &env{srv: srv, repo: repo, bus: bus}
}

func readJSONFile(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *env) post(t *testing.T, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case string:
		buf.WriteString(b)
	default:
		if err := json.NewEncoder(&buf).Encode(b); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Post(e.srv.URL+path, "application/json", &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// assertWithinContract fails when got has a field the contract payload lacks,
// recursing into objects and into the first element of arrays.
func assertWithinContract(t *testing.T, path string, got, contract any) {
	t.Helper()
	switch g := got.(type) {
	case map[string]any:
		c, ok := contract.(map[string]any)
		if !ok {
			return
		}
		for k, v := range g {
			cv, known := c[k]
			if !known {
				t.Errorf("field %s.%s is not in the contract payload", path, k)
				continue
			}
			assertWithinContract(t, path+"."+k, v, cv)
		}
	case []any:
		c, ok := contract.([]any)
		if ok && len(g) > 0 && len(c) > 0 {
			assertWithinContract(t, path+"[]", g[0], c[0])
		}
	}
}

func assertHasFields(t *testing.T, got map[string]any, fields ...string) {
	t.Helper()
	var missing []string
	for _, f := range fields {
		if _, ok := got[f]; !ok {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("response is missing required fields: %s (got %v)", strings.Join(missing, ", "), got)
	}
}
