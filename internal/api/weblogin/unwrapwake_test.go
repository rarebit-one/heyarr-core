package weblogin_test

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/enrolment"

	"github.com/rarebit-one/heyarr-core/internal/api/weblogin"
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
// heyarr asks the notify plane — with its bearer — to wake THAT cert's user at
// exactly the relay/session pointer, nothing else, relaying the plane's count.
func TestUnwrapWakeWakesTheUsersPhone(t *testing.T) {
	h := newPushHarness(t)
	cert, priv := h.enrolledDevice(t)
	user := h.pinnedUserIDs(t)[0]

	const relayBase, session = "https://heyarr.test/pair", "sess-offload-1"
	resp := h.postWake(t, `{"cert":"`+cert+`","possession":"`+prove(t, priv, cert)+`","relay_base":"`+relayBase+`","session":"`+session+`"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("unwrap-wake = %d (%s), want 200", resp.StatusCode, b)
	}
	if got := wokenOf(t, resp); got != 1 {
		t.Fatalf("woken = %d, want 1", got)
	}

	got := h.plane.received()
	if len(got) != 1 {
		t.Fatalf("plane received %d calls, want 1", len(got))
	}
	c := got[0]
	if c.Route != "/v1/enqueue-unwrap" || c.Bearer != planeBearer {
		t.Fatalf("call = %s with bearer %q, want /v1/enqueue-unwrap with the configured bearer", c.Route, c.Bearer)
	}
	want := map[string]string{"user_id": user, "relay_base": relayBase, "session": session}
	if len(c.Body) != len(want) {
		t.Fatalf("enqueue-unwrap body %v, want exactly %v", c.Body, want)
	}
	for k, v := range want {
		if c.Body[k] != v {
			t.Fatalf("enqueue-unwrap %s = %q, want %q", k, c.Body[k], v)
		}
	}
}

// wokenOf decodes an unwrap-wake response's woken count.
func wokenOf(t *testing.T, resp *http.Response) int {
	t.Helper()
	var out struct {
		Woken int `json:"woken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Woken
}

// TestUnwrapWakeUnsubscribedUserWakesNobody: when the plane wakes nobody (no
// phone subscribed) the desktop gets a 200 with woken:0, not an error — it can
// still try the LAN-direct path.
func TestUnwrapWakeUnsubscribedUserWakesNobody(t *testing.T) {
	h := newPushHarness(t)
	h.plane.woken = 0
	cert, priv := h.enrolledDevice(t)

	resp := h.postWake(t, `{"cert":"`+cert+`","possession":"`+prove(t, priv, cert)+`","relay_base":"r","session":"s"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unwrap-wake = %d, want 200", resp.StatusCode)
	}
	if got := wokenOf(t, resp); got != 0 {
		t.Fatalf("woken = %d, want 0", got)
	}
}

// TestUnwrapWakeWithoutAPlaneWakesNobody: a node with no notify plane still
// authenticates the request, then answers woken:0 — the same answer as an
// unsubscribed user, so the desktop keeps polling the relay.
func TestUnwrapWakeWithoutAPlaneWakesNobody(t *testing.T) {
	h := newHarness(t)
	cert, priv := h.enrolledDevice(t)

	resp, err := h.ts.Client().Post(h.ts.URL+weblogin.UnwrapWakePrefix, "application/json",
		strings.NewReader(`{"cert":"`+cert+`","possession":"`+prove(t, priv, cert)+`","relay_base":"r","session":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unwrap-wake without a plane = %d, want 200", resp.StatusCode)
	}
	if got := wokenOf(t, resp); got != 0 {
		t.Fatalf("woken = %d, want 0", got)
	}
}

// TestUnwrapWakePlaneFailureIsBadGateway: a plane that refuses the bearer or
// errors is a 502 to the desktop — the wake failed, not the request — and the
// answer never carries the plane's error text.
func TestUnwrapWakePlaneFailureIsBadGateway(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			h := newPushHarness(t)
			h.plane.setStatus(code)
			cert, priv := h.enrolledDevice(t)
			resp := h.postWake(t, `{"cert":"`+cert+`","possession":"`+prove(t, priv, cert)+`","relay_base":"r","session":"s"}`)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("unwrap-wake with the plane answering %d = %d, want 502", code, resp.StatusCode)
			}
			b, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(b), planeBearer) || strings.Contains(string(b), "notify") {
				t.Fatalf("502 body leaked plane detail: %q", b)
			}
		})
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

// wakeStatus POSTs an unwrap-wake body and returns the status.
func (h *pushHarness) wakeStatus(t *testing.T, body string) int {
	t.Helper()
	resp := h.postWake(t, body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestUnwrapWakeAuthenticatesPossession covers the check → possession → commit
// chain (void-which-binds-go#70): a proof may ride in `possession` or inside
// `cert` as the Device credential; a proof from another key, a malformed one,
// one given twice, or none at all (a bare cert — the pre-v0.18 wire, refused
// since the gen2 cutover, ADR-0022) is the opaque 401.
func TestUnwrapWakeAuthenticatesPossession(t *testing.T) {
	h := newPushHarness(t)
	cert, priv := h.enrolledDevice(t)
	_, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	const tail = `","relay_base":"r","session":"s"}`
	cases := []struct {
		name string
		body string
		want int
	}{
		{"proof field", `{"cert":"` + cert + `","possession":"` + prove(t, priv, cert) + tail, http.StatusOK},
		{"device credential", `{"cert":"` + cert + enrolment.CredentialSeparator + prove(t, priv, cert) + tail, http.StatusOK},
		{"bare cert", `{"cert":"` + cert + tail, http.StatusUnauthorized},
		{"foreign proof", `{"cert":"` + cert + `","possession":"` + prove(t, otherPriv, cert) + tail, http.StatusUnauthorized},
		{"malformed proof", `{"cert":"` + cert + `","possession":"not-a-proof` + tail, http.StatusUnauthorized},
		{"proof twice", `{"cert":"` + cert + enrolment.CredentialSeparator + prove(t, priv, cert) + `","possession":"` + prove(t, priv, cert) + tail, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.wakeStatus(t, tc.body); got != tc.want {
				t.Fatalf("unwrap-wake = %d, want %d", got, tc.want)
			}
		})
	}
}
