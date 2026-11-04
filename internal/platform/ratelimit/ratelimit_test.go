package ratelimit_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/ratelimit"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newClock() *clock { return &clock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)} }

func TestLimiterAllowsTheBurstThenDeniesUntilTokensRefill(t *testing.T) {
	c := newClock()
	l := ratelimit.New(2, 3, c.Now) // 2 tokens/s, burst 3

	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("request %d inside the burst was denied", i+1)
		}
	}
	if l.Allow("a") {
		t.Fatal("request over the burst was allowed")
	}
	c.Advance(500 * time.Millisecond) // one token back
	if !l.Allow("a") {
		t.Fatal("request after refill was denied")
	}
	if l.Allow("a") {
		t.Fatal("only one token should have been refilled")
	}
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	l := ratelimit.New(1, 1, newClock().Now)

	if !l.Allow("a") || l.Allow("a") {
		t.Fatal("key a: want first allowed, second denied")
	}
	if !l.Allow("b") {
		t.Fatal("key b must have its own bucket")
	}
}

func TestParseCIDRsAcceptsPrefixesAndPlainAddresses(t *testing.T) {
	got, err := ratelimit.ParseCIDRs(" 10.0.0.0/8, 192.168.1.5 ,")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, err %v", got, err)
	}
	if !got[0].Contains(netip.MustParseAddr("10.9.9.9")) || !got[1].Contains(netip.MustParseAddr("192.168.1.5")) {
		t.Fatalf("prefixes = %v", got)
	}
	if none, err := ratelimit.ParseCIDRs(""); err != nil || len(none) != 0 {
		t.Fatalf("empty = %v, %v", none, err)
	}
	if _, err := ratelimit.ParseCIDRs("not-a-cidr"); err == nil {
		t.Fatal("want an error for an invalid entry")
	}
}

func request(remote, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestClientIPIgnoresForwardedForFromUntrustedPeers(t *testing.T) {
	trusted, _ := ratelimit.ParseCIDRs("10.0.0.0/8")

	got := ratelimit.ClientIP(request("203.0.113.7:5000", "1.2.3.4"), trusted)

	if got != "203.0.113.7" {
		t.Fatalf("ClientIP = %s, want the peer address", got)
	}
}

func TestClientIPUsesTheRightmostUntrustedHopBehindATrustedProxy(t *testing.T) {
	trusted, _ := ratelimit.ParseCIDRs("10.0.0.0/8")

	// The leftmost entry is whatever the client wrote; the proxy appended 198.51.100.9.
	got := ratelimit.ClientIP(request("10.0.0.2:5000", "6.6.6.6, 198.51.100.9, 10.0.0.3"), trusted)

	if got != "198.51.100.9" {
		t.Fatalf("ClientIP = %s, want 198.51.100.9", got)
	}
}

func TestClientIPFallsBackToThePeerWhenForwardedForIsUnusable(t *testing.T) {
	trusted, _ := ratelimit.ParseCIDRs("10.0.0.0/8")

	for _, xff := range []string{"", "10.0.0.9"} {
		if got := ratelimit.ClientIP(request("10.0.0.2:5000", xff), trusted); got != "10.0.0.2" {
			t.Fatalf("xff %q: ClientIP = %s, want the peer", xff, got)
		}
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestMiddlewareAnswers429WithRetryAfterWhenTheIPExceedsItsBudget(t *testing.T) {
	h := ratelimit.Middleware(ratelimit.New(1, 2, newClock().Now), nil)(okHandler())

	for i := 0; i < 2; i++ {
		if rec := serve(h, request("203.0.113.7:1", "")); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i+1, rec.Code)
		}
	}
	rec := serve(h, request("203.0.113.7:2", ""))

	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d, Retry-After = %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := serve(h, request("203.0.113.8:1", "")); rec.Code != http.StatusOK {
		t.Fatalf("another IP was limited: %d", rec.Code)
	}
}

func TestForgedForwardedForFromAnUntrustedClientDoesNotEscapeTheLimit(t *testing.T) {
	trusted, _ := ratelimit.ParseCIDRs("10.0.0.0/8")
	h := ratelimit.Middleware(ratelimit.New(1, 2, newClock().Now), trusted)(okHandler())

	var last int
	for _, forged := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"} {
		last = serve(h, request("203.0.113.7:1", forged)).Code
	}

	if last != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: a rotating forged header must not mint new buckets", last)
	}
}

func TestMiddlewareKeysByTheForwardedClientWhenThePeerIsATrustedProxy(t *testing.T) {
	trusted, _ := ratelimit.ParseCIDRs("10.0.0.0/8")
	h := ratelimit.Middleware(ratelimit.New(1, 1, newClock().Now), trusted)(okHandler())

	a1 := serve(h, request("10.0.0.2:1", "198.51.100.1")).Code
	a2 := serve(h, request("10.0.0.2:1", "198.51.100.1")).Code
	b1 := serve(h, request("10.0.0.2:1", "198.51.100.2")).Code

	if a1 != http.StatusOK || a2 != http.StatusTooManyRequests || b1 != http.StatusOK {
		t.Fatalf("a1 = %d, a2 = %d, b1 = %d; want 200, 429, 200", a1, a2, b1)
	}
}

func TestMiddlewareDoesNotLimitExemptPaths(t *testing.T) {
	h := ratelimit.Middleware(ratelimit.New(1, 1, newClock().Now), nil, ratelimit.Path("/healthz"))(okHandler())

	for i := 0; i < 5; i++ {
		r := request("203.0.113.7:1", "")
		r.URL.Path = "/healthz"
		if rec := serve(h, r); rec.Code != http.StatusOK {
			t.Fatalf("healthz request %d: %d", i+1, rec.Code)
		}
	}
}

func TestClientIPSkipsMalformedHopsAndNeverFallsBackToTheProxy(t *testing.T) {
	trusted, _ := ratelimit.ParseCIDRs("10.0.0.0/8")

	if got := ratelimit.ClientIP(request("10.0.0.2:5000", "garbage, 198.51.100.9"), trusted); got != "198.51.100.9" {
		t.Fatalf("ClientIP = %q, want the rightmost valid untrusted hop", got)
	}
	if got := ratelimit.ClientIP(request("10.0.0.2:5000", "198.51.100.9, garbage"), trusted); got != "" {
		t.Fatalf("ClientIP = %q, want empty: a malformed hop next to the proxy fails closed", got)
	}
	if got := ratelimit.ClientIP(request("10.0.0.2:5000", "198.51.100.9, garbage, 10.0.0.7"), trusted); got != "" {
		t.Fatalf("ClientIP = %q, want empty: a malformed hop behind a trusted hop fails closed", got)
	}
	if got := ratelimit.ClientIP(request("10.0.0.2:5000", "garbage"), trusted); got != "" {
		t.Fatalf("ClientIP = %q, want empty: the proxy bucket is shared by everyone", got)
	}
}

func TestMiddlewareAnswers400WhenTheTrustedProxySentOnlyGarbageAndSpendsNoBudget(t *testing.T) {
	trusted, _ := ratelimit.ParseCIDRs("10.0.0.0/8")
	h := ratelimit.Middleware(ratelimit.New(1, 1, newClock().Now), trusted)(okHandler())

	for i := 0; i < 3; i++ {
		if rec := serve(h, request("10.0.0.2:1", "garbage")); rec.Code != http.StatusBadRequest {
			t.Fatalf("garbage XFF: status %d, want 400", rec.Code)
		}
	}
	if rec := serve(h, request("10.0.0.2:1", "")); rec.Code != http.StatusOK {
		t.Fatalf("the proxy itself was limited: %d", rec.Code)
	}
}

func TestIPv6ClientsShareTheirSlash64Bucket(t *testing.T) {
	h := ratelimit.Middleware(ratelimit.New(1, 1, newClock().Now), nil)(okHandler())

	first := serve(h, request("[2001:db8:1:2::1]:1", "")).Code
	sameNet := serve(h, request("[2001:db8:1:2:ffff::9]:1", "")).Code
	otherNet := serve(h, request("[2001:db8:1:3::1]:1", "")).Code

	if first != http.StatusOK || sameNet != http.StatusTooManyRequests || otherNet != http.StatusOK {
		t.Fatalf("first = %d, sameNet = %d, otherNet = %d; want 200, 429, 200", first, sameNet, otherNet)
	}
}

func TestLimiterRefusesNewKeysOnceTheMapIsFull(t *testing.T) {
	l := ratelimit.New(1, 5, newClock().Now, ratelimit.WithMaxKeys(2))

	if !l.Allow("a") || !l.Allow("b") {
		t.Fatal("the first two keys must fit")
	}
	if l.Allow("c") {
		t.Fatal("a third key must not grow the map")
	}
	if !l.Allow("a") {
		t.Fatal("a known key keeps its bucket")
	}
}

func TestInternalPrefixExemptsOnlyInternalPeersWithoutForwardingHeaders(t *testing.T) {
	internal, _ := ratelimit.ParseCIDRs("172.30.0.0/24")
	h := ratelimit.Middleware(ratelimit.New(1, 1, newClock().Now), nil, ratelimit.InternalPrefix("/demo/mkt/", internal, nil))(okHandler())
	call := func(remote, xff, path string) int {
		r := request(remote, xff)
		r.URL.Path = path
		return serve(h, r).Code
	}

	for i := 0; i < 4; i++ {
		if code := call("172.30.0.5:1", "", "/demo/mkt/pvt/orders/x/invoice"); code != http.StatusOK {
			t.Fatalf("internal callback %d: %d", i+1, code)
		}
	}
	call("203.0.113.7:1", "", "/demo/mkt/pvt/orders/x/invoice")
	if code := call("203.0.113.7:1", "", "/demo/mkt/pvt/orders/x/invoice"); code != http.StatusTooManyRequests {
		t.Fatalf("an external caller must stay limited: %d", code)
	}
	call("172.30.0.1:1", "198.51.100.1", "/demo/mkt/pvt/orders/x/invoice")
	if code := call("172.30.0.1:1", "198.51.100.1", "/demo/mkt/pvt/orders/x/invoice"); code != http.StatusTooManyRequests {
		t.Fatalf("a request that came through the proxy must stay limited: %d", code)
	}
	if !ratelimit.IsInternal(request("172.30.0.5:1", ""), internal, nil) || ratelimit.IsInternal(request("172.30.0.5:1", "1.1.1.1"), internal, nil) {
		t.Fatal("IsInternal must require an internal peer and no forwarding header")
	}
}

func TestIsInternalRejectsAnyForwardingHeaderEvenWhenEmpty(t *testing.T) {
	internal, _ := ratelimit.ParseCIDRs("172.30.0.0/24")

	for _, header := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip"} {
		r := request("172.30.0.5:1", "")
		r.Header[header] = []string{""}
		if ratelimit.IsInternal(r, internal, nil) {
			t.Fatalf("an empty %s header must make the request non internal", header)
		}
	}
	r := request("172.30.0.5:1", "")
	r.Header.Add("X-Forwarded-For", "")
	r.Header.Add("X-Forwarded-For", "1.1.1.1")
	if ratelimit.IsInternal(r, internal, nil) {
		t.Fatal("a second X-Forwarded-For line must make the request non internal")
	}
}

func TestIsInternalNeverCountsATrustedProxyAsInternal(t *testing.T) {
	internal, _ := ratelimit.ParseCIDRs("172.30.0.0/24,172.30.1.0/24")
	trusted, _ := ratelimit.ParseCIDRs("172.30.0.1/32")

	if ratelimit.IsInternal(request("172.30.0.1:1", ""), internal, trusted) {
		t.Fatal("the gateway listed in TRUSTED_PROXIES is the public path, never internal")
	}
	if !ratelimit.IsInternal(request("172.30.1.5:1", ""), internal, trusted) {
		t.Fatal("another peer of the internal networks stays internal")
	}
}
