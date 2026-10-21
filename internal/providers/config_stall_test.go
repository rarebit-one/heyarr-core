package providers

import (
	"testing"
	"time"
)

// stall_after (#811): empty is the default (zero), "0" is off (negative),
// anything else must be a positive duration.
func TestResolveStallAfter(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": 0, "0": -1, "6h": 6 * time.Hour, " 90m ": 90 * time.Minute} {
		got, err := resolveStallAfter("tr", raw)
		if err != nil || got != want {
			t.Errorf("resolveStallAfter(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"-1h", "soon", "6"} {
		if _, err := resolveStallAfter("tr", raw); err == nil {
			t.Errorf("resolveStallAfter(%q) accepted a non-positive or malformed duration", raw)
		}
	}
}
