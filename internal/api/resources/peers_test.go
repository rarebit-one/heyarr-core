// Every HTTP response here is closed by the t.Cleanup the harness registers,
// which bodyclose cannot see through — the same exemption harness_test.go
// carries for the same reason.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// Peer enrolment over HTTP (§26, ADR-0012, M4-04).
//
// These tests are about the wire contract: which status each refusal gets,
// what a re-registration answers, and what is and is not in the body. The
// rules themselves are tested against a real database in
// internal/peer/membership.

func newKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return identity.FormatPublicKey(pub)
}

// seedSelf writes the row ADR-0010 creates at first start. The harness does
// not run a controller, so nothing else would, and half the refusals here are
// about the self peer existing.
func (h *harness) seedSelf() *harness {
	h.t.Helper()
	h.exec(`INSERT INTO peers (id, name, site, mode, is_self, enrolled_at, created_at)
		VALUES ('01990000-0000-7000-8000-0000000000a1', 'this-node', 'site-a', 'full', 1, ?, ?)`,
		seedTime, seedTime)
	return h
}

func (h *harness) postPeer(t *testing.T, body map[string]string) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return h.do(http.MethodPost, "/api/v1/peers", "", bytes.NewReader(buf))
}

func decodePeer(t *testing.T, resp *http.Response) resources.Peer {
	t.Helper()
	var p resources.Peer
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decoding a peer: %v", err)
	}
	return p
}

func TestEnrollingAPeerPinsItsKey(t *testing.T) {
	h := newHarness(t).seedSelf()
	pub := newKey(t)

	resp := h.postPeer(t, map[string]string{
		"name": "peer-b", "site": "site-b",
		"endpoint": "https://b.example:8385", "public_key": pub,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /peers = %d, want 201", resp.StatusCode)
	}
	created := decodePeer(t, resp)
	if created.PublicKey == nil || *created.PublicKey != pub {
		t.Errorf("public_key = %v, want %q", created.PublicKey, pub)
	}
	if created.IsSelf {
		t.Error("an enrolled peer was marked as this node")
	}
	if loc := resp.Header.Get("Location"); !strings.HasSuffix(loc, created.ID) {
		t.Errorf("Location = %q, want it to name the created peer", loc)
	}

	// It is in the list, alongside self, with the key intact.
	var page struct {
		Items []resources.Peer `json:"items"`
	}
	decode(t, h, "/api/v1/peers", &page)
	if len(page.Items) != 2 {
		t.Fatalf("%d peers, want 2 (self and peer-b)", len(page.Items))
	}
	var found bool
	for _, p := range page.Items {
		if p.ID == created.ID {
			found = true
			if p.PublicKey == nil || *p.PublicKey != pub {
				t.Errorf("the listed peer's key is %v, want %q", p.PublicKey, pub)
			}
		}
	}
	if !found {
		t.Error("the enrolled peer is not in the list")
	}

	// And by name, which is what an operator holds.
	byName := h.get("/api/v1/peers/peer-b")
	if byName.StatusCode != http.StatusOK {
		t.Fatalf("GET /peers/peer-b = %d, want 200", byName.StatusCode)
	}
	if got := decodePeer(t, byName); got.ID != created.ID {
		t.Errorf("GET by name found %s, want %s", got.ID, created.ID)
	}
}

// TestReRegisteringMovesTheEndpointAndAnswers200: nothing was created, so the
// status must not say something was.
func TestReRegisteringMovesTheEndpointAndAnswers200(t *testing.T) {
	h := newHarness(t).seedSelf()
	pub := newKey(t)

	first := decodePeer(t, h.postPeer(t, map[string]string{
		"name": "peer-b", "site": "site-b", "endpoint": "https://b.example:8385", "public_key": pub,
	}))

	resp := h.postPeer(t, map[string]string{
		"name": "peer-b", "site": "site-b", "endpoint": "https://moved.example:8385", "public_key": pub,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-registration = %d, want 200", resp.StatusCode)
	}
	moved := decodePeer(t, resp)
	if moved.ID != first.ID {
		t.Errorf("the peer id changed: %s -> %s", first.ID, moved.ID)
	}
	if moved.Endpoint == nil || *moved.Endpoint != "https://moved.example:8385" {
		t.Errorf("endpoint = %v, want the new one", moved.Endpoint)
	}
	if moved.PublicKey == nil || *moved.PublicKey != pub {
		t.Errorf("the identity moved with the endpoint: %v", moved.PublicKey)
	}
	if !moved.EnrolledAt.Equal(first.EnrolledAt) {
		t.Errorf("enrolled_at changed: %v -> %v", first.EnrolledAt, moved.EnrolledAt)
	}
}

// TestEachEnrolmentRefusalGetsItsOwnStatus. One case per refusal: they are
// different mistakes, they need different statuses, and a client that saw one
// status for all of them could not tell a typo from a collision.
func TestEachEnrolmentRefusalGetsItsOwnStatus(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the request body, after establishing whatever state
		// makes it a refusal.
		setup func(t *testing.T, h *harness) map[string]string
		// method and path override the default POST /peers.
		delete string
		want   int
		says   string
	}{
		{
			name: "a malformed public key",
			setup: func(*testing.T, *harness) map[string]string {
				return map[string]string{"name": "peer-b", "public_key": "ed25519:not-hex"}
			},
			want: http.StatusBadRequest,
			says: "not hex",
		},
		{
			name: "a public key of the wrong length",
			setup: func(*testing.T, *harness) map[string]string {
				return map[string]string{"name": "peer-b", "public_key": "ed25519:" + strings.Repeat("ab", 16)}
			},
			want: http.StatusBadRequest,
			says: "16 bytes",
		},
		{
			name: "no public key",
			setup: func(*testing.T, *harness) map[string]string {
				return map[string]string{"name": "peer-b", "endpoint": "https://b.example:8385"}
			},
			want: http.StatusBadRequest,
			says: "registered by its public key",
		},
		{
			name: "a key already registered to another peer",
			setup: func(t *testing.T, h *harness) map[string]string {
				pub := newKey(t)
				h.postPeer(t, map[string]string{"name": "peer-b", "public_key": pub})
				return map[string]string{"name": "peer-c", "public_key": pub}
			},
			want: http.StatusConflict,
			says: "peer-b",
		},
		{
			name: "a name already taken",
			setup: func(t *testing.T, h *harness) map[string]string {
				h.postPeer(t, map[string]string{"name": "peer-b", "public_key": newKey(t)})
				return map[string]string{"name": "peer-b", "public_key": newKey(t)}
			},
			want: http.StatusConflict,
			says: "already registered under this name",
		},
		{
			name:   "removing this node",
			setup:  func(*testing.T, *harness) map[string]string { return nil },
			delete: "/api/v1/peers/this-node",
			want:   http.StatusConflict,
			says:   "cannot remove its own membership",
		},
		{
			name:   "removing a peer that does not exist",
			setup:  func(*testing.T, *harness) map[string]string { return nil },
			delete: "/api/v1/peers/never-heard-of-it",
			want:   http.StatusNotFound,
			says:   "no peer is registered",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t).seedSelf()
			body := tc.setup(t, h)

			var resp *http.Response
			if tc.delete != "" {
				resp = h.do(http.MethodDelete, tc.delete, "", nil)
			} else {
				resp = h.postPeer(t, body)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			var doc map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
				t.Fatal(err)
			}
			detail, _ := doc["detail"].(string)
			if !strings.Contains(strings.ToLower(detail), strings.ToLower(tc.says)) {
				t.Errorf("the problem document does not say %q: %q", tc.says, detail)
			}
		})
	}
}

// TestTheAPICannotRegisterASecondSelf. There is no field for it on the wire —
// which is the point — so this asserts that the one route that writes peers
// rows cannot produce one, however it is called.
func TestTheAPICannotRegisterASecondSelf(t *testing.T) {
	h := newHarness(t).seedSelf()
	// is_self is not part of the request schema. Sending it anyway is what an
	// attacker or a hopeful client would do, and the decoder refuses unknown
	// fields — so the request does not quietly become a create-with-is_self
	// that the handler then has to remember to strip.
	pub := newKey(t)
	buf := []byte(`{"name":"impostor","public_key":"` + pub + `","is_self":true}`)
	resp := h.do(http.MethodPost, "/api/v1/peers", "", bytes.NewReader(buf))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /peers with is_self = %d, want 400", resp.StatusCode)
	}

	// The same registration without the field is accepted, and the peer that
	// comes back is NOT this node. Without this half the 400 above could be
	// any rejection at all.
	resp = h.postPeer(t, map[string]string{"name": "impostor", "public_key": pub})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /peers = %d, want 201", resp.StatusCode)
	}
	created := decodePeer(t, resp)
	if created.IsSelf {
		t.Fatal("the API registered another machine as this node")
	}

	var page struct {
		Items []resources.Peer `json:"items"`
	}
	decode(t, h, "/api/v1/peers", &page)
	selves := 0
	for _, p := range page.Items {
		if p.IsSelf {
			selves++
		}
	}
	if selves != 1 {
		t.Errorf("%d peers claim to be this node, want 1", selves)
	}
}
