// Guest capability gating (ADR-0094): a guest admitted from a trusted source is
// backed by an access lease carrying browse + play + subtitle, and nothing more.
// These drive the REAL router: browse and play reach the routes their capability
// covers; every write is refused with the stable, machine-readable capability
// reason code; and the two per-identity read chokepoints stay closed.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/guest"
)

// A guest from an allowed source (the httptest listener is loopback, inside the
// default trusted net) can browse the shared library and play from it — the two
// capabilities the lease grants that reach a route.
func TestGuestFromAnAllowedSourceCanBrowseAndPlay(t *testing.T) {
	h := newHarness(t, withAuth, withGuest).seed()

	// Browse: the read floor its browse capability covers — the library, and the
	// asset listings a subtitle is fetched from (subtitles ride the asset/blob
	// read routes, so the subtitle capability is a read a guest already holds).
	for _, path := range []string{"/api/v1/works", "/api/v1/assets", "/api/v1/works/" + work1ID + "/assets"} {
		if resp := h.get(path); resp.StatusCode != http.StatusOK {
			t.Errorf("guest GET %s = %d, want 200 (browse)", path, resp.StatusCode)
		}
	}

	// Play: POST /playback is write-scoped, but the guest's play capability
	// reaches EXACTLY it. A created session proves the whole flow worked for a
	// credential-less guest, not merely that the middleware let it past.
	resp, got := h.startPlayback(t, asset1ID, device1ID, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("guest POST /playback = %d, want 201 (play): %s", resp.StatusCode, h.body(resp))
	}
	if got.SessionID == "" {
		t.Error("guest playback opened no session")
	}
}

// Every write is refused, and each refusal carries the SAME stable code a client
// can branch on — not the route's prose. want (create desired), monitor (patch
// desired), acquire (reconcile), follow, subtitle want/backfill and enrich are
// all closed to a guest, whose lease carries none of them.
func TestGuestIsRefusedEveryWriteWithACapabilityCode(t *testing.T) {
	h := newHarness(t, withAuth, withGuest).seed()

	// A desired id to aim the per-item write routes at. The write guard runs
	// before the handler looks the id up, so the id need not resolve — the point
	// is the refusal, which is the same for a real id and a missing one.
	const someID = "01990000-0000-7000-8000-00000000d001"

	cases := []struct {
		name, method, path, body string
	}{
		{"want (create desired)", http.MethodPost, "/api/v1/desired", `{"work_id":"x","kind":"movie"}`},
		{"monitor (patch desired)", http.MethodPatch, "/api/v1/desired/" + someID, `{"monitor":true}`},
		{"acquire (reconcile)", http.MethodPost, "/api/v1/desired/" + someID + "/reconcile", `{}`},
		{"acquire (adopt)", http.MethodPost, "/api/v1/desired/" + someID + "/acquisitions", `{}`},
		{"follow", http.MethodPost, "/api/v1/followed-sources", `{"intent":"x"}`},
		{"subtitle want", http.MethodPost, "/api/v1/subtitles/want", `{"work_id":"x"}`},
		{"subtitle backfill", http.MethodPost, "/api/v1/subtitles/backfill", `{}`},
		{"enrich backfill", http.MethodPost, "/api/v1/enrich/backfill", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(tc.method, tc.path, "", strings.NewReader(tc.body))
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("guest %s %s = %d, want 403", tc.method, tc.path, resp.StatusCode)
			}
			p := decodeProblem(t, resp, h.body(resp))
			if p.Type != problem.TypeForbidden {
				t.Errorf("problem type = %q, want %q", p.Type, problem.TypeForbidden)
			}
			if p.Code != guest.ReasonCapabilityDenied {
				t.Errorf("problem code = %q, want %q — a guest write refusal must be machine-readable",
					p.Code, guest.ReasonCapabilityDenied)
			}
		})
	}
}

// The two per-identity read chokepoints (§72) stay closed to the lease-minted
// guest: consumption history is somebody's, and a guest is nobody. The reader
// token reaching the same route proves the 403 is about being a guest, not the
// route being shut.
func TestGuestIsRefusedTheConsumptionChokepoints(t *testing.T) {
	h := newHarness(t, withAuth, withGuest).seed().seedBrowse().seedContinue()

	for _, path := range []string{"/api/v1/consumption/sessions", "/api/v1/consumption/continue"} {
		if resp := h.get(path); resp.StatusCode != http.StatusForbidden {
			t.Errorf("guest GET %s = %d, want 403 (per-identity chokepoint)", path, resp.StatusCode)
		}
	}

	tok := h.mint("reader", auth.ScopeRead)
	for _, path := range []string{"/api/v1/consumption/sessions", "/api/v1/consumption/continue"} {
		if resp := h.do(http.MethodGet, path, tok.Secret, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("reader GET %s = %d, want 200 — the route is open, the guest is not", path, resp.StatusCode)
		}
	}
}

// A guest lease is minted with EXACTLY browse+play+subtitle, principal "guest",
// and a 1h life — the shape ADR-0094 fixes. Reading it back off the wire (the
// playback credential is scoped to the lease's read floor) would not show the
// capabilities, so this asserts the capability set through the play gate: the
// play capability is present (playback succeeds) and no write capability leaked
// (every write above is refused). The precise lease row is asserted in the
// leases and guest package tests, on the injected clock.
func TestGuestLeaseGrantsPlayButNoWrite(t *testing.T) {
	h := newHarness(t, withAuth, withGuest).seed()

	if resp, _ := h.startPlayback(t, asset1ID, device1ID, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("play capability missing: POST /playback = %d", resp.StatusCode)
	}
	// A representative write stays closed: the lease is play, not write.
	resp := h.do(http.MethodPost, "/api/v1/desired", "", strings.NewReader(`{"work_id":"x","kind":"movie"}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a guest wrote desired state: %d", resp.StatusCode)
	}
	if p := decodeProblem(t, resp, h.body(resp)); p.Code != guest.ReasonCapabilityDenied {
		t.Errorf("write refusal code = %q, want %q", p.Code, guest.ReasonCapabilityDenied)
	}
}
