package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
)

// The personal profile serves peer membership over the client API (#762).
//
// TestProfileRouting walks the route table; this file drives the routes on a
// real server with authentication on, because the thing that was broken was
// not a missing pattern in a list but `mnemosyne peers add` answering "no route
// matches /api/v1/peers" against a live node — and the fix is only a fix if an
// admin token can enrol a sibling, a read token can list it, and nothing from
// the rest of the resource API came along for the ride.

// noPeerPresented is the client listener's honest answer: no connection on it
// presents a peer certificate.
func noPeerPresented(*http.Request) ([]byte, bool) { return nil, false }

// wirePeer is the subset of the /peers wire shape these assertions read.
type wirePeer struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	IsSelf    bool    `json:"is_self"`
	PublicKey *string `json:"public_key"`
}

type wirePeerPage struct {
	Items []wirePeer `json:"items"`
}

// registerSelf gives the node under test the self record Run's bootstrap
// would have written (ADR-0010), so the list has the row `peers ping` pins.
func registerSelf(t *testing.T, h *peerSurfaceHarness, name string) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.members.Register(context.Background(), membership.Registration{
		Name: name, Site: "site-a", PublicKey: pub, IsSelf: true,
	}); err != nil {
		t.Fatalf("registering the self peer: %v", err)
	}
	return pub
}

// call issues one JSON request against the harness and returns the status,
// the raw body and the response headers.
func call(t *testing.T, h *peerSurfaceHarness, method, path, token string, body any) (int, []byte, http.Header) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, h.server.URL+path, payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := assertClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw, resp.Header
}

func TestPersonalProfileServesPeerMembership(t *testing.T) {
	c := newProfileHandler(t, config.MnemosyneDefaults())
	h := newProfileSurfaceHarness(t, c, noPeerPresented)
	selfKey := registerSelf(t, h, "peer-under-test")
	admin := h.token(t, "admin", auth.ScopeAdmin)
	reader := h.token(t, "reader", auth.ScopeRead)
	peers := httpapi.APIPrefix + "/peers"

	// The list, under the read floor: one row, and it is this node.
	status, raw, _ := call(t, h, http.MethodGet, peers, reader, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s on the personal profile answered %d, want 200\n%s", peers, status, raw)
	}
	var page wirePeerPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decoding the peer list: %v\n%s", err, raw)
	}
	if len(page.Items) != 1 || !page.Items[0].IsSelf || page.Items[0].Name != "peer-under-test" {
		t.Fatalf("the peer list should hold exactly the self peer, got %s", raw)
	}
	if want := identity.FormatPublicKey(selfKey); page.Items[0].PublicKey == nil || *page.Items[0].PublicKey != want {
		t.Errorf("the self peer's public key is %v, want %q — it is what the other site enrols", page.Items[0].PublicKey, want)
	}

	// Enrolment is admin, in both directions (ADR-0012).
	siblingPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sibling := map[string]string{
		"name": "peer-b", "site": "site-b", "mode": "full",
		"public_key": identity.FormatPublicKey(siblingPub),
	}
	if status, raw, _ := call(t, h, http.MethodPost, peers, reader, sibling); status != http.StatusForbidden {
		t.Fatalf("POST %s with a read token answered %d, want 403\n%s", peers, status, raw)
	}
	status, raw, hdr := call(t, h, http.MethodPost, peers, admin, sibling)
	if status != http.StatusCreated {
		t.Fatalf("POST %s with an admin token answered %d, want 201\n%s", peers, status, raw)
	}
	var enrolled wirePeer
	if err := json.Unmarshal(raw, &enrolled); err != nil {
		t.Fatal(err)
	}
	if enrolled.IsSelf || enrolled.Name != "peer-b" || enrolled.ID == "" {
		t.Fatalf("enrolment answered %s", raw)
	}
	if got, want := hdr.Get("Location"), peers+"/"+enrolled.ID; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	// The single-peer view resolves by name as well as id — it is what
	// `peers show` and `peers ping` read the pinned key out of.
	for _, ref := range []string{enrolled.ID, "peer-b"} {
		status, raw, _ := call(t, h, http.MethodGet, peers+"/"+ref, reader, nil)
		if status != http.StatusOK {
			t.Errorf("GET %s/%s answered %d, want 200\n%s", peers, ref, status, raw)
		}
	}
	status, raw, _ = call(t, h, http.MethodGet, peers, reader, nil)
	if err := json.Unmarshal(raw, &page); err != nil || status != http.StatusOK {
		t.Fatalf("listing after enrolment: %d %v\n%s", status, err, raw)
	}
	if len(page.Items) != 2 {
		t.Fatalf("after enrolling a sibling the list holds %d peers, want 2\n%s", len(page.Items), raw)
	}

	// Reconcile on demand is `write`; a read token may not ask for it.
	if status, raw, _ := call(t, h, http.MethodPost, peers+"/peer-b/reconcile", reader, nil); status != http.StatusForbidden {
		t.Errorf("POST %s/peer-b/reconcile with a read token answered %d, want 403\n%s", peers, status, raw)
	}

	// Revocation is admin, and it answers with the record it removed.
	if status, raw, _ := call(t, h, http.MethodDelete, peers+"/peer-b", reader, nil); status != http.StatusForbidden {
		t.Errorf("DELETE %s/peer-b with a read token answered %d, want 403\n%s", peers, status, raw)
	}
	status, raw, _ = call(t, h, http.MethodDelete, peers+"/peer-b", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE %s/peer-b with an admin token answered %d, want 200\n%s", peers, status, raw)
	}

	// Nothing else from the resource API came along: the media surface, and
	// the token admin surface the CLI serves by opening the database, stay
	// unmounted on this profile.
	for _, path := range []string{"/libraries", "/works", "/assets", "/jobs", "/tokens", "/devices", "/replicas"} {
		status, raw, _ := call(t, h, http.MethodGet, httpapi.APIPrefix+path, admin, nil)
		if status != http.StatusNotFound {
			t.Errorf("GET %s%s on the personal profile answered %d, want 404 (media-only)\n%s",
				httpapi.APIPrefix, path, status, raw)
		}
	}
}

// The media profile is what it was: the peers routes moved into their own mount
// function and are still registered, beside the library they always sat next to.
func TestMediaProfileStillServesPeersBesideTheLibrary(t *testing.T) {
	h := newPeerSurfaceHarness(t, noPeerPresented)
	registerSelf(t, h, "peer-under-test")
	reader := h.token(t, "reader", auth.ScopeRead)
	for _, path := range []string{"/peers", "/peers/peer-under-test", "/libraries", "/works"} {
		status, raw, _ := call(t, h, http.MethodGet, httpapi.APIPrefix+path, reader, nil)
		if status != http.StatusOK {
			t.Errorf("GET %s%s on the media profile answered %d, want 200\n%s", httpapi.APIPrefix, path, status, raw)
		}
	}
}
