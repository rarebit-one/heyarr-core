// Guest mode (ADR-0074): a credential-less caller is admitted as an anonymous,
// read-only Guest over the shared library when the operator enables the mode,
// refused otherwise, and kept off the per-identity read surface either way.
//
//nolint:bodyclose // responses are closed by h.do's t.Cleanup
package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/config"
)

func withGuestEnabled(c *config.Config) { c.HTTP.Guest.Enabled = true }

// With guest mode OFF (the default), a request with no credential is refused
// exactly as before: there is no anonymous read.
func TestGuestDisabledRefusesTheCredentiallessReader(t *testing.T) {
	h := newHarness(t).start(t)

	resp := h.do(t, http.MethodGet, "/api/v1/probe", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET without a credential and guest off = %d, want 401", resp.StatusCode)
	}
}

// With guest mode ON, the same credential-less request is admitted and can read
// the shared-library surface.
func TestGuestEnabledAdmitsTheCredentiallessReader(t *testing.T) {
	h := newHarness(t, withGuestEnabled).start(t)

	resp := h.do(t, http.MethodGet, "/api/v1/probe", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET without a credential and guest on = %d, want 200", resp.StatusCode)
	}
}

// A Guest holds only read: a write route is a 403, not a 200, and not a 401.
// The distinction matters — "you may not" is a different answer from "who are
// you", and a Guest is somebody, just somebody without write.
func TestGuestCannotWrite(t *testing.T) {
	h := newHarness(t, withGuestEnabled).start(t)

	resp := h.do(t, http.MethodPost, "/api/v1/probe", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST as a guest = %d, want 403", resp.StatusCode)
	}
}

// A Guest is refused the per-identity read surface even though it holds read:
// RefuseGuest is the guard a scope check cannot be.
func TestGuestRefusedPerIdentityReads(t *testing.T) {
	h := newHarness(t, withGuestEnabled).start(t)

	resp := h.do(t, http.MethodGet, "/api/v1/personal", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("guest GET of a per-identity read = %d, want 403", resp.StatusCode)
	}
}

// The same per-identity route is reachable by an enrolled reader: RefuseGuest
// refuses a Guest and nobody else. This is what proves the 403 above is about
// being a Guest, not about the route being closed to everyone.
func TestNonGuestReaderReachesPerIdentityReads(t *testing.T) {
	h := newHarness(t, withGuestEnabled).start(t)
	tok := h.mint(t, "reader", auth.ScopeRead)

	resp := h.do(t, http.MethodGet, "/api/v1/personal", tok.Secret)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read-token GET of a per-identity read = %d, want 200", resp.StatusCode)
	}
}

// A rejected credential is never quietly downgraded to a Guest: a bad token is a
// 401 even with guest mode on, so enabling Guest cannot mask an auth failure.
func TestGuestDoesNotDowngradeARejectedCredential(t *testing.T) {
	h := newHarness(t, withGuestEnabled).start(t)

	resp := h.do(t, http.MethodGet, "/api/v1/probe", "not-a-real-token")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a bad token with guest on = %d, want 401 (never a silent guest)", resp.StatusCode)
	}
}

// /system reports whether guest mode is available, the client-facing signal.
func TestSystemReportsGuestMode(t *testing.T) {
	h := newHarness(t, withGuestEnabled).start(t)

	resp := h.do(t, http.MethodGet, "/api/v1/system", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("guest GET /system = %d, want 200", resp.StatusCode)
	}
	var info httpapi.SystemInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decoding /system: %v", err)
	}
	if !info.Guest.Enabled {
		t.Fatal("/system did not report guest mode as enabled")
	}
}
