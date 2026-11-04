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
// the first untrusted one. Entries to its left were written by the client
// and are never used. An unparseable chain falls back to the peer.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	peer := ap.Addr().Unmap()
	if !isTrusted(peer, trusted) {
		return peer.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for _, hop := range slices.Backward(hops) {
		addr, err := netip.ParseAddr(strings.TrimSpace(hop))
		if err != nil {
			return peer.String()
		}
		if addr = addr.Unmap(); !isTrusted(addr, trusted) {
			return addr.String()
		}
	}
	return peer.String()
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	return slices.ContainsFunc(trusted, func(p netip.Prefix) bool { return p.Contains(addr) })
}

// Middleware answers 429 once a client IP spends its budget. Requests whose
// path is listed in exempt (such as health checks) are never limited.
func Middleware(l *Limiter, trusted []netip.Prefix, exempt ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !slices.Contains(exempt, r.URL.Path) && !l.Allow(ClientIP(r, trusted)) {
				w.Header().Set("Retry-After", "1")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"too many requests, slow down"}}` + "\n"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
