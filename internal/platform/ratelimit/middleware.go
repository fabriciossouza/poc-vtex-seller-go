package ratelimit

import (
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// ParseCIDRs reads a comma-separated list of CIDRs or plain addresses.
func ParseCIDRs(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			out = append(out, prefix)
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q", entry)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// ClientIP returns the peer address, unless the peer is a trusted proxy: then
// it walks X-Forwarded-For from the right, skipping trusted hops, and returns
// the first valid untrusted one. Entries to its left were written by the
// client and are never used. A chain with no usable hop yields "" when the
// header is present but malformed, because answering with the proxy address
// would put every such client in one shared bucket; an absent header or a
// chain made only of trusted hops means the proxy itself is the client.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	peer := ap.Addr().Unmap()
	if !isTrusted(peer, trusted) {
		return peer.String()
	}
	raw := strings.TrimSpace(strings.Join(r.Header.Values("X-Forwarded-For"), ","))
	if raw == "" {
		return peer.String()
	}
	sawValid := false
	for _, hop := range slices.Backward(strings.Split(raw, ",")) {
		addr, err := netip.ParseAddr(strings.TrimSpace(hop))
		if err != nil {
			continue
		}
		sawValid = true
		if addr = addr.Unmap(); !isTrusted(addr, trusted) {
			return addr.String()
		}
	}
	if sawValid {
		return peer.String()
	}
	return ""
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	return slices.ContainsFunc(trusted, func(p netip.Prefix) bool { return p.Contains(addr) })
}

// IsInternal reports whether the request came straight from one of the
// internal networks, not through the public proxy: the peer must be inside
// nets and no forwarding header may be present (a proxy always adds one).
func IsInternal(r *http.Request, nets []netip.Prefix) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" || r.Header.Get("X-Real-Ip") != "" {
		return false
	}
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	return err == nil && isTrusted(ap.Addr().Unmap(), nets)
}

// limitKey groups IPv6 clients by /64, since one subscriber usually owns the
// whole prefix and could otherwise mint a bucket per address.
func limitKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil || addr.Is4() {
		return ip
	}
	return netip.PrefixFrom(addr, 64).Masked().String()
}

// Exemption decides that a request is never limited.
type Exemption func(*http.Request) bool

// Path exempts requests for exactly this path, such as a health check.
func Path(path string) Exemption {
	return func(r *http.Request) bool { return r.URL.Path == path }
}

// InternalPrefix exempts requests under prefix that come from internal networks.
func InternalPrefix(prefix string, nets []netip.Prefix) Exemption {
	return func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, prefix) && IsInternal(r, nets) }
}

// Middleware answers 429 once a client IP spends its budget, and 400 when a
// trusted proxy forwarded a header that names no client.
func Middleware(l *Limiter, trusted []netip.Prefix, exempt ...Exemption) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if slices.ContainsFunc(exempt, func(e Exemption) bool { return e(r) }) {
				next.ServeHTTP(w, r)
				return
			}
			ip := ClientIP(r, trusted)
			if ip == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_forwarded_for", "X-Forwarded-For does not name a client")
				return
			}
			if !l.Allow(limitKey(ip)) {
				w.Header().Set("Retry-After", "1")
				writeJSONError(w, http.StatusTooManyRequests, "rate_limited", "too many requests, slow down")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"` + message + `"}}` + "\n"))
}
