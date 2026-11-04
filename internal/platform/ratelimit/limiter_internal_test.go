package ratelimit

import (
	"testing"
	"time"
)

func TestIdleBucketsAreForgottenSoMemoryStaysBounded(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	l := New(1, 2, func() time.Time { return now })
	for i := 0; i < 1000; i++ {
		l.Allow(string(rune('a'+i%26)) + time.Duration(i).String())
	}

	now = now.Add(time.Hour)
	l.Allow("fresh")

	if len(l.buckets) != 1 {
		t.Fatalf("buckets = %d, want only the fresh one", len(l.buckets))
	}
}
