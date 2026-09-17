package weblogin_test

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/weblogin"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
)

// postWake calls POST /v1/unwrap-wake with a JSON body and returns the response.
func (h *pushHarness) postWake(t *testing.T, body string) *http.Response {
	t.Helper()
	resp, err := h.ts.Client().Post(h.ts.URL+weblogin.UnwrapWakePrefix, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestUnwrapWakeWakesTheUsersPhone is the load-bearing behaviour: an enrolled
// device (the offload desktop) asks to wake its user's phone for an unwrap, and
// the subscribed phone receives exactly one opaque unwrap ping carrying the
// relay/session pointer — nothing else.
func TestUnwrapWakeWakesTheUsersPhone(t *testing.T) {
	h := newPushHarness(t)
	cert, _ := h.enrolledDevice(t)
	// The phone registers its wake endpoint (same user, cert-authed).
	h.subscribe(t, cert, "https://ntfy.example/phone-topic")

	const relayBase, session = "https://heyarr.test/pair", "sess-offload-1"
	resp := h.postWake(t, `{"cert":"`+cert+`","relay_base":"`+relayBase+`","session":"`+session+`"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("unwrap-wake = %d (%s), want 200", resp.StatusCode, b)
	}
	var out struct {
		Woken int `json:"woken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Woken != 1 {
		t.Fatalf("woken = %d, want 1", out.Woken)
	}

	got := h.ch.captured()
	if len(got) != 1 {
		t.Fatalf("captured %d pings, want 1", len(got))
	}
	tuple := got[0].Tuple
	// The ping is the opaque unwrap pointer: it carries the relay session and
	// nothing secret (there is no secret — the desktop's signed request rides the
	// relay, not the ping).
	if !strings.HasPrefix(tuple, "voidbind:unwrap?") || !strings.Contains(tuple, session) {
		t.Fatalf("ping tuple %q is not the opaque unwrap pointer for the session", tuple)
	}
	if strings.Contains(tuple, cert) {
		t.Fatalf("ping tuple leaked the cert: %q", tuple)
	}
}

// TestUnwrapWakeUnsubscribedUserWakesNobody: an enrolled desktop with no
// subscribed phone wakes zero devices — a 200 with woken:0, not an error (the
// desktop can still try the LAN-direct path).
func TestUnwrapWakeUnsubscribedUserWakesNobody(t *testing.T) {
	h := newPushHarness(t)
	cert, _ := h.enrolledDevice(t) // enrolled, but no phone subscribed

	resp := h.postWake(t, `{"cert":"`+cert+`","relay_base":"r","session":"s"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unwrap-wake = %d, want 200", resp.StatusCode)
	}
	if got := h.ch.captured(); len(got) != 0 {
		t.Fatalf("captured %d pings, want 0", len(got))
	}
}

// TestUnwrapWakeRejectsAStranger: a cert for a user this node has not pinned is
// refused with an opaque 401 — only enrolled devices may wake, and only their own
// user's phones.
func TestUnwrapWakeRejectsAStranger(t *testing.T) {
	h := newPushHarness(t)
	_, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	strangerPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	strangerCert, err := enrolment.SignCert(userPriv, strangerPub, "", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	resp := h.postWake(t, `{"cert":"`+strangerCert+`","relay_base":"r","session":"s"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stranger unwrap-wake = %d, want 401", resp.StatusCode)
	}
}

// TestUnwrapWakeValidatesInput: a missing relay/session is a 400, a non-POST a
// 405 — before any wake.
func TestUnwrapWakeValidatesInput(t *testing.T) {
	h := newPushHarness(t)
	cert, _ := h.enrolledDevice(t)

	resp := h.postWake(t, `{"cert":"`+cert+`","session":"s"}`) // no relay_base
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing relay_base = %d, want 400", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, h.ts.URL+weblogin.UnwrapWakePrefix, nil)
	if err != nil {
		t.Fatal(err)
	}
	getResp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = getResp.Body.Close() }()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET unwrap-wake = %d, want 405", getResp.StatusCode)
	}
}
