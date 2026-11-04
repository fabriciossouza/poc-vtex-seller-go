package main

import (
	"strings"
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

var required = map[string]string{
	"ERP_URL":                   "http://erp:8080",
	"TMS_URL":                   "http://tms:8080",
	"MARKETPLACE_ALLOWED_HOSTS": "seller:8080, 127.0.0.1:8080",
}

func with(extra map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range required {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestDefaultsApplyWhenOnlyTheRequiredVariablesAreSet(t *testing.T) {
	s, err := loadSettings(env(required))
	if err != nil {
		t.Fatal(err)
	}

	if s.listenAddr != ":8080" || s.redisAddr != "127.0.0.1:6379" || s.resetInterval != time.Hour {
		t.Fatalf("settings = %+v", s)
	}
	c := s.app
	if c.Retry.MaxAttempts != 4 || c.Breaker.FailureThreshold != 3 || c.Breaker.OpenTimeout != 8*time.Second || c.DependencyTimeout != 3*time.Second {
		t.Fatalf("resilience defaults = %+v / %+v / %v", c.Retry, c.Breaker, c.DependencyTimeout)
	}
	if len(c.MarketplaceAllowedHosts) != 2 || c.MarketplaceAllowedHosts[1] != "127.0.0.1:8080" {
		t.Fatalf("allowlist = %v", c.MarketplaceAllowedHosts)
	}
	if c.MarketplaceEndpoint != "http://seller:8080/demo/mkt" {
		t.Fatalf("endpoint = %q, want it derived from the first allowed host", c.MarketplaceEndpoint)
	}
	if c.RateLimitPerSecond != 10 || c.RateLimitBurst != 30 || len(c.TrustedProxies) != 0 {
		t.Fatalf("rate limit = %v/%d, proxies %v", c.RateLimitPerSecond, c.RateLimitBurst, c.TrustedProxies)
	}
}

func TestRequiredVariablesAreNamedWhenMissing(t *testing.T) {
	for _, name := range []string{"ERP_URL", "TMS_URL", "MARKETPLACE_ALLOWED_HOSTS"} {
		vars := with(nil)
		delete(vars, name)
		if _, err := loadSettings(env(vars)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: err = %v", name, err)
		}
	}
}

func TestOverridesAreParsed(t *testing.T) {
	s, err := loadSettings(env(with(map[string]string{
		"LISTEN_ADDR": ":9000", "RETRY_MAX_ATTEMPTS": "6", "BREAKER_OPEN_TIMEOUT": "20s",
		"RATE_LIMIT_RPS": "2.5", "RATE_LIMIT_BURST": "7", "TRUSTED_PROXIES": "172.16.0.0/12,10.0.0.1",
		"RESET_INTERVAL": "0", "MARKETPLACE_ENDPOINT": "http://seller:8080/other",
	})))
	if err != nil {
		t.Fatal(err)
	}

	if s.listenAddr != ":9000" || s.app.Retry.MaxAttempts != 6 || s.app.Breaker.OpenTimeout != 20*time.Second ||
		s.app.RateLimitPerSecond != 2.5 || s.app.RateLimitBurst != 7 || len(s.app.TrustedProxies) != 2 ||
		s.resetInterval != 0 || s.app.MarketplaceEndpoint != "http://seller:8080/other" {
		t.Fatalf("settings = %+v", s)
	}
}

func TestInvalidValuesAreRejectedWithTheVariableName(t *testing.T) {
	for name, value := range map[string]string{
		"RETRY_MAX_ATTEMPTS":   "0",
		"BREAKER_OPEN_TIMEOUT": "soon",
		"RATE_LIMIT_RPS":       "-1",
		"RATE_LIMIT_BURST":     "x",
		"TRUSTED_PROXIES":      "not-a-cidr",
		"DEPENDENCY_TIMEOUT":   "0s",
	} {
		_, err := loadSettings(env(with(map[string]string{name: value})))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: err = %v", name, value, err)
		}
	}
}
