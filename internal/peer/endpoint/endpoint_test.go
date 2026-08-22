package endpoint_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
)

// The cases are enumerated one by one rather than collapsed into "invalid
// input is rejected", because each of them fails for a different reason and a
// single table row asserting "some error" would keep passing if three of the
// four stopped being checked (#169).

func TestNormaliseAccepts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"a bare host:port gains the only scheme this fabric speaks", "192.168.1.50:8443", "https://192.168.1.50:8443"},
		{"a hostname and port", "b.example:8385", "https://b.example:8385"},
		{"an IPv6 literal keeps its brackets", "[2001:db8::1]:8443", "https://[2001:db8::1]:8443"},
		{"a scheme-qualified endpoint survives byte for byte", "https://b.example:8385", "https://b.example:8385"},
		{"the socket a single-host deployment derives for itself", "unix:///var/lib/heyarr/heyarr.sock", "unix:///var/lib/heyarr/heyarr.sock"},
		{"a URL without a port keeps the implied one", "https://b.example", "https://b.example"},
		{"a trailing slash is not a path", "https://b.example:8385/", "https://b.example:8385"},
		{"surrounding whitespace is not a value", "  https://b.example:8385  ", "https://b.example:8385"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := endpoint.Normalise(tc.in)
			if err != nil {
				t.Fatalf("Normalise(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Normalise(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormaliseRefuses(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// says is what the operator must be able to read out of the refusal.
		says []string
	}{
		{"the scheme the inter-peer path does not speak", "http://", []string{"http", "https"}},
		{"http with a host is still a plaintext network hop", "http://b.example:8385", []string{"http://b.example:8385", "https"}},
		{"a socket with no path", "unix://", []string{"unix://", "socket path"}},
		{"a relative socket path", "unix://heyarr.sock", []string{"relative"}},
		{"a port with no machine attached to it", ":8443", []string{":8443"}},
		{"a port that is not a number", "host:notaport", []string{"host:notaport", "notaport"}},
		{"nothing at all", "", []string{"empty"}},
		{"whitespace is nothing at all", "   ", []string{"empty"}},
		{"a host with no port", "b.example", []string{"b.example"}},
		{"a scheme nothing here speaks", "ftp://b.example:21", []string{"ftp"}},
		{"an origin is not a path", "https://b.example:8385/peer/v1", []string{"/peer/v1"}},
		{"credentials belong to a password scheme, not this one", "https://user:pw@b.example:8385", []string{"credentials"}},
		{"a port out of range", "b.example:70000", []string{"70000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := endpoint.Normalise(tc.in)
			if err == nil {
				t.Fatalf("Normalise(%q) = %q, want a refusal", tc.in, got)
			}
			if !errors.Is(err, endpoint.ErrMalformed) {
				t.Errorf("the refusal does not wrap ErrMalformed, so no caller can map it: %v", err)
			}
			for _, want := range tc.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
			// Every refusal shows what a right value looks like. An error that
			// says a value is wrong and not what a right one is leaves the
			// operator guessing a second time.
			if !strings.Contains(err.Error(), endpoint.Example) {
				t.Errorf("the refusal carries no example: %v", err)
			}
		})
	}
}

// TestNormaliseIsIdempotent: what add stores is what a later add re-reads, so
// re-registering a peer with the value `peers list` printed cannot drift.
func TestNormaliseIsIdempotent(t *testing.T) {
	for _, in := range []string{"192.168.1.50:8443", "https://b.example:8385", "[2001:db8::1]:8443"} {
		once, err := endpoint.Normalise(in)
		if err != nil {
			t.Fatalf("Normalise(%q): %v", in, err)
		}
		twice, err := endpoint.Normalise(once)
		if err != nil {
			t.Fatalf("Normalise(%q): %v", once, err)
		}
		if once != twice {
			t.Errorf("Normalise(%q) = %q, then %q", in, once, twice)
		}
	}
}
