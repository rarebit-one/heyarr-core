// Every HTTP response in this file is closed by the t.Cleanup that the harness
// (or the local helper) registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by t.Cleanup
package resources_test

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/voidbind-go/enrolment"

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// TestPhoneSelfEnrolsAndReadsButDoesNotWrite is the acceptance for ADR-0067,
// end to end through the real router: an admin pins a user; a device holding a
// cert that user signed (what pairing hands a phone) and a fresh possession
// proof — both minted with voidbind-go's enrolment package, the phone's code —
// POSTs /enrol with no credential; it then reads /api/v1/works under the Device
// scheme and is refused a write, because enrolment grants the read floor and
// nothing more (ADR-0065).
func TestPhoneSelfEnrolsAndReadsButDoesNotWrite(t *testing.T) {
	h := newHarness(t, withAuth)
	admin := h.mint("admin", auth.ScopeAdmin)

	// The admin's one act: pin the user (ADR-0032's enrol-before-trust gate).
	u, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	resp := h.do(http.MethodPost, "/api/v1/identities/users", admin.Secret,
		strings.NewReader(fmt.Sprintf(`{"public_key":%q,"name":"owner"}`, u.UserID())))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("pin user: status = %d (body: %s)", resp.StatusCode, h.body(resp))
	}

	// What pairing leaves on the phone: a device key and a user-signed cert.
	devicePub, devicePriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := enrolment.SignCert(userPriv, devicePub, "", fixedTime, 0)
	if err != nil {
		t.Fatal(err)
	}
	proof := func(priv ed25519.PrivateKey, over string) string {
		t.Helper()
		p, err := enrolment.SignPossession(priv, over, fixedTime, 0)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	enrolBody := func(cert, proof string) *strings.Reader {
		return strings.NewReader(fmt.Sprintf(`{"cert":%q,"proof":%q,"name":"phone"}`, cert, proof))
	}
	deviceHeader := func() string {
		return "Device " + cert + "~" + proof(devicePriv, cert)
	}
	withDevice := func(method, path string, body io.Reader) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, h.http.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", deviceHeader())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	// Before enrolment the very same credential is refused: the cert alone is
	// not trust (ADR-0048 step 4).
	if resp := withDevice(http.MethodGet, "/api/v1/works", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unenrolled device read: status = %d, want 401", resp.StatusCode)
	}

	// Self-enrol. No Authorization header: the request IS the credential.
	resp = h.do(http.MethodPost, "/enrol", "", enrolBody(cert, proof(devicePriv, cert)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /enrol: status = %d, want 201 (body: %s)", resp.StatusCode, h.body(resp))
	}
	var enrolled struct {
		DeviceKey string    `json:"device_key"`
		User      string    `json:"user"`
		Name      string    `json:"name"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(h.body(resp), &enrolled); err != nil {
		t.Fatal(err)
	}
	if enrolled.User != u.UserID() || enrolled.Name != "phone" || enrolled.ExpiresAt.IsZero() {
		t.Fatalf("enrolled = %+v", enrolled)
	}
	if got := resp.Header.Get("Location"); got != "/api/v1/identities/devices/"+enrolled.DeviceKey {
		t.Fatalf("Location = %q", got)
	}

	// Read: 200 under the Device scheme, with a fresh proof.
	if resp := withDevice(http.MethodGet, "/api/v1/works", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("enrolled device read: status = %d, want 200 (body: %s)", resp.StatusCode, h.body(resp))
	}
	// Write: 403 — enrolled, authenticated, and not authorised. Not a 401: the
	// device is known; it simply does not carry write (ADR-0065).
	resp = withDevice(http.MethodPost, "/api/v1/libraries", strings.NewReader(`{"name":"phone-made","content_type":"movie"}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("enrolled device write: status = %d, want 403 (body: %s)", resp.StatusCode, h.body(resp))
	}
	// And the admin surface is out of reach the same way.
	if resp := withDevice(http.MethodGet, "/api/v1/identities/users", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("enrolled device admin read: status = %d, want 403", resp.StatusCode)
	}

	// Re-submitting the same key is idempotent: 200, the same row, no duplicate.
	resp = h.do(http.MethodPost, "/enrol", "", enrolBody(cert, proof(devicePriv, cert)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-POST /enrol: status = %d, want 200 (body: %s)", resp.StatusCode, h.body(resp))
	}
	var again struct {
		DeviceKey string `json:"device_key"`
	}
	if err := json.Unmarshal(h.body(resp), &again); err != nil || again.DeviceKey != enrolled.DeviceKey {
		t.Fatalf("re-POST returned %+v (%v), want %s", again, err, enrolled.DeviceKey)
	}
	if n := h.countRows(t, `SELECT count(*) FROM device_identities`); n != 1 {
		t.Fatalf("device rows after re-submit = %d, want 1", n)
	}

	// Every refusal is the same opaque 401 the Device scheme gives — nothing
	// for a probing caller to learn from.
	_, strangerPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	otherPub, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	strangerCert, err := enrolment.SignCert(strangerPriv, otherPub, "", fixedTime, 0)
	if err != nil {
		t.Fatal(err)
	}
	otherCert, err := enrolment.SignCert(userPriv, otherPub, "", fixedTime, 0)
	if err != nil {
		t.Fatal(err)
	}
	refusals := map[string]*strings.Reader{
		"unpinned user":         enrolBody(strangerCert, proof(otherPriv, strangerCert)),
		"proof by wrong key":    enrolBody(otherCert, proof(devicePriv, otherCert)),
		"proof over wrong cert": enrolBody(otherCert, proof(otherPriv, cert)),
		"empty proof":           enrolBody(otherCert, ""),
		"garbage cert":          enrolBody("nope", proof(otherPriv, otherCert)),
	}
	for name, body := range refusals {
		resp := h.do(http.MethodPost, "/enrol", "", body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
			continue
		}
		if detail := string(h.body(resp)); !strings.Contains(detail, "the presented credential was rejected") {
			t.Errorf("%s: detail leaks the reason: %s", name, detail)
		}
	}
	if n := h.countRows(t, `SELECT count(*) FROM device_identities`); n != 1 {
		t.Fatalf("device rows after refusals = %d, want 1", n)
	}
	// A body that is not a credential at all is a 400, not a 401.
	if resp := h.do(http.MethodPost, "/enrol", "", strings.NewReader(`{"cert":"x","proof":"y","typo":1}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d, want 400", resp.StatusCode)
	}
}
