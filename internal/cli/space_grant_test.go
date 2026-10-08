package cli

import (
	"testing"
	"time"
)

// --expires takes a duration from now or an RFC 3339 time, and never the past.
func TestParseExpiry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"720h":                 now.Add(720 * time.Hour),
		"2026-11-01T00:00:00Z": time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseExpiry(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseExpiry(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"-1h", "0s", "2020-01-01T00:00:00Z", "next week"} {
		if _, err := parseExpiry(in, now); err == nil {
			t.Errorf("parseExpiry(%q) was accepted", in)
		}
	}
}
