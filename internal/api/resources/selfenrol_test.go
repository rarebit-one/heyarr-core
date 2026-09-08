// Every HTTP response in this file is closed by the t.Cleanup that the harness
// (or the local helper) registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by t.Cleanup
package resources_test

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/voidbind-go/enrolment"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
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

	// A cert from a user this node has NOT pinned is refused: the pin is the
	// trust root (ADR-0032, ADR-0068). (A pinned user's genesis-signed cert, by
	// contrast, authenticates on first contact — membership_test.go.)
	_, unpinnedPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	unpinnedCert, err := enrolment.SignCert(unpinnedPriv, devicePub, "", fixedTime, 0)
	if err != nil {
		t.Fatal(err)
	}
	{
		req, _ := http.NewRequest(http.MethodGet, h.http.URL+"/api/v1/works", nil)
		req.Header.Set("Authorization", "Device "+unpinnedCert+"~"+proof(devicePriv, unpinnedCert))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unpinned user's device read: status = %d, want 401", resp.StatusCode)
		}
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

// TestEnrolmentCarriesRecoveryEncryptionKey is the acceptance for part 2 of
// rarebit-one/heyarr-mobile#41 (Option A): the device-enrolment response carries
// the user identity's X25519 recovery encryption PUBLIC key so the enrolling
// device can wrap new personal-state spaces for recovery. The key is registered
// when the operator pins the user; the paper recovery SECRET never enters the
// server, and this test asserts the response carries the public recipient and
// nothing derived from a secret.
func TestEnrolmentCarriesRecoveryEncryptionKey(t *testing.T) {
	h := newHarness(t, withAuth)
	admin := h.mint("admin", auth.ScopeAdmin)

	// A recovery encryption key pair. Only the PUBLIC half is ever given to the
	// server (as the person's paper recovery secret would be, out of band); the
	// private half stands in for that secret and must never surface on the wire.
	recovPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	recoveryPub := encryption.FormatPublicKey(recovPriv.PublicKey().Bytes())
	recoverySecretHex := hex.EncodeToString(recovPriv.Bytes())

	// enrol pins a user (optionally with a recovery encryption key), then
	// self-enrols a fresh device under it and returns the decoded response plus
	// its raw body.
	enrol := func(t *testing.T, name, recovery string) (map[string]any, string) {
		t.Helper()
		u, userPriv, err := enrolment.GenerateUserIdentity()
		if err != nil {
			t.Fatal(err)
		}
		var pinBody string
		if recovery == "" {
			pinBody = fmt.Sprintf(`{"public_key":%q,"name":%q}`, u.UserID(), name)
		} else {
			pinBody = fmt.Sprintf(`{"public_key":%q,"recovery_encryption_key":%q,"name":%q}`, u.UserID(), recovery, name)
		}
		resp := h.do(http.MethodPost, "/api/v1/identities/users", admin.Secret, strings.NewReader(pinBody))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("pin user: status = %d (body: %s)", resp.StatusCode, h.body(resp))
		}
		devicePub, devicePriv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := enrolment.SignCert(userPriv, devicePub, "", fixedTime, 0)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := enrolment.SignPossession(devicePriv, cert, fixedTime, 0)
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"cert":%q,"proof":%q,"name":"phone"}`, cert, proof)
		resp = h.do(http.MethodPost, "/enrol", "", strings.NewReader(body))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST /enrol: status = %d, want 201 (body: %s)", resp.StatusCode, h.body(resp))
		}
		raw := string(h.body(resp))
		var out map[string]any
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		return out, raw
	}

	// With a recovery key registered, the response carries the PUBLIC key verbatim.
	out, raw := enrol(t, "owner-with-recovery", recoveryPub)
	if got := out["recovery_encryption_key"]; got != recoveryPub {
		t.Fatalf("recovery_encryption_key = %v, want %q", got, recoveryPub)
	}
	if !strings.HasPrefix(recoveryPub, "x25519:") {
		t.Fatalf("recovery key %q is not an x25519 public recipient", recoveryPub)
	}
	// The secret must NEVER appear anywhere in the response body — not as the field
	// value, not smuggled into another field.
	if strings.Contains(raw, recoverySecretHex) {
		t.Fatalf("the recovery SECRET leaked into the enrolment response: %s", raw)
	}

	// Without a recovery key, the field is omitted entirely (omitempty) — an
	// identity that predates recovery-wrap carries none.
	out, raw = enrol(t, "owner-no-recovery", "")
	if got, ok := out["recovery_encryption_key"]; ok {
		t.Fatalf("recovery_encryption_key present without a registered key: %v (body: %s)", got, raw)
	}
}

// TestEnrolUserRejectsMalformedRecoveryKey asserts a non-empty recovery
// encryption key that is not a well-formed x25519 public key is a 400, so garbage
// never reaches the enrolment response.
func TestEnrolUserRejectsMalformedRecoveryKey(t *testing.T) {
	h := newHarness(t, withAuth)
	admin := h.mint("admin", auth.ScopeAdmin)
	u, _, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"public_key":%q,"recovery_encryption_key":"ed25519:abc","name":"owner"}`, u.UserID())
	resp := h.do(http.MethodPost, "/api/v1/identities/users", admin.Secret, strings.NewReader(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed recovery key: status = %d, want 400 (body: %s)", resp.StatusCode, h.body(resp))
	}
}
