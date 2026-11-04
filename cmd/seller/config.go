package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/bootstrap"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/platform/ratelimit"
)

type settings struct {
	listenAddr    string
	redisAddr     string
	resetInterval time.Duration
	app           bootstrap.Config
}

type reader struct {
	getenv func(string) string
	err    error
}

func (r *reader) str(name, fallback string) string {
	if v := strings.TrimSpace(r.getenv(name)); v != "" {
		return v
	}
	return fallback
}

func (r *reader) required(name string) string {
	v := r.str(name, "")
	if v == "" && r.err == nil {
		r.err = fmt.Errorf("%s is required", name)
	}
	return v
}

func (r *reader) fail(name string, err error) {
	if r.err == nil {
		r.err = fmt.Errorf("%s: %w", name, err)
	}
}

func (r *reader) duration(name string, fallback time.Duration, allowZero bool) time.Duration {
	raw := r.str(name, "")
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if raw == "0" {
		d, err = 0, nil
	}
	if err != nil || d < 0 || (d == 0 && !allowZero) {
		r.fail(name, fmt.Errorf("invalid duration %q", raw))
	}
	return d
}

func (r *reader) integer(name string, fallback int) int {
	raw := r.str(name, "")
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		r.fail(name, fmt.Errorf("must be a positive integer, got %q", raw))
	}
	return n
}

func (r *reader) number(name string, fallback float64) float64 {
	raw := r.str(name, "")
	if raw == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f <= 0 {
		r.fail(name, fmt.Errorf("must be a positive number, got %q", raw))
	}
	return f
}

func loadSettings(getenv func(string) string) (settings, error) {
	r := &reader{getenv: getenv}
	var s settings
	s.listenAddr = r.str("LISTEN_ADDR", ":8080")
	s.redisAddr = r.str("REDIS_ADDR", "127.0.0.1:6379")
	s.resetInterval = r.duration("RESET_INTERVAL", time.Hour, true)

	c := &s.app
	c.DBPath = r.str("DB_PATH", "seller.db")
	c.ERPURL = r.required("ERP_URL")
	c.TMSURL = r.required("TMS_URL")
	for _, h := range strings.Split(r.required("MARKETPLACE_ALLOWED_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.MarketplaceAllowedHosts = append(c.MarketplaceAllowedHosts, h)
		}
	}
	if len(c.MarketplaceAllowedHosts) > 0 {
		c.MarketplaceEndpoint = r.str("MARKETPLACE_ENDPOINT", "http://"+c.MarketplaceAllowedHosts[0]+"/demo/mkt")
	}
	c.DependencyTimeout = r.duration("DEPENDENCY_TIMEOUT", 3*time.Second, false)
	c.Retry.MaxAttempts = r.integer("RETRY_MAX_ATTEMPTS", 4)
	c.Retry.BaseDelay = r.duration("RETRY_BASE_DELAY", 200*time.Millisecond, false)
	c.Retry.MaxDelay = r.duration("RETRY_MAX_DELAY", 2*time.Second, false)
	c.Breaker.FailureThreshold = r.integer("BREAKER_FAILURE_THRESHOLD", 3)
	c.Breaker.OpenTimeout = r.duration("BREAKER_OPEN_TIMEOUT", 8*time.Second, false)
	c.QueueBlock = time.Second
	c.RateLimitPerSecond = r.number("RATE_LIMIT_RPS", 10)
	c.RateLimitBurst = r.integer("RATE_LIMIT_BURST", 30)
	proxies, err := ratelimit.ParseCIDRs(r.str("TRUSTED_PROXIES", ""))
	if err != nil {
		r.fail("TRUSTED_PROXIES", err)
	}
	c.TrustedProxies = proxies

	if r.err != nil {
		return settings{}, r.err
	}
	return s, nil
}
