//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// newIdentityKey returns a fresh Ed25519 keypair rendered the way the API expects.
func newIdentityKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv, identity.FormatPublicKey(pub)
}

// TestIdentityRoutesAreAdminInBothDirections is the scope contract: pinning a
// user decides who may authenticate as a principal on this node, so reads and
// writes alike require admin, and a write token — which could otherwise mint
// itself an identity — is refused.
func TestIdentityRoutesAreAdminInBothDirections(t *testing.T) {
	h := newHarness(t, withAuth)
	reader := h.mint("reader", auth.ScopeRead)
	writer := h.mint("writer", auth.ScopeWrite)
	admin := h.mint("admin", auth.ScopeAdmin)

	_, _, userKey := newIdentityKey(t)
	body := fmt.Sprintf(`{"public_key":%q,"name":"phone-owner"}`, userKey)

	for _, tc := range []struct {
		who   string
		token string
		want  int
	}{
		{"read", reader.Secret, http.StatusForbidden},
		{"write", writer.Secret, http.StatusForbidden},
		{"admin", admin.Secret, http.StatusCreated},
	} {
		resp := h.do(http.MethodPost, "/api/v1/identities/users", tc.token, strings.NewReader(body))
		if resp.StatusCode != tc.want {
			t.Errorf("POST /identities/users as %s: status = %d, want %d", tc.who, resp.StatusCode, tc.want)
		}
	}

	// Listing is admin too — the list is a map of who this node trusts.
	if resp := h.do(http.MethodGet, "/api/v1/identities/users", writer.Secret, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /identities/users as write: status = %d, want 403", resp.StatusCode)
	}
	if resp := h.do(http.MethodGet, "/api/v1/identities/users", admin.Secret, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /identities/users as admin: status = %d, want 200", resp.StatusCode)
	}
}

// TestEnrolUserThenDeviceThenRevoke walks the whole admin flow through the real
// router and asserts the identities the store recorded, not the status codes
// alone: the device the API lists is the one whose cert was posted.
func TestEnrolUserThenDeviceThenRevoke(t *testing.T) {
	h := newHarness(t, withAuth)
	admin := h.mint("admin", auth.ScopeAdmin)

	userPub, userPriv, userKey := newIdentityKey(t)
	devicePub, _, deviceKey := newIdentityKey(t)

	// Pin the user.
	resp := h.do(http.MethodPost, "/api/v1/identities/users", admin.Secret,
		strings.NewReader(fmt.Sprintf(`{"public_key":%q,"name":"owner"}`, userKey)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("enrol user: status = %d, want 201", resp.StatusCode)
	}
	var user struct {
		PublicKey string `json:"public_key"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(h.body(resp), &user); err != nil {
		t.Fatal(err)
	}
	if user.PublicKey != userKey {
		t.Fatalf("enrolled user key = %q, want %q", user.PublicKey, userKey)
	}
	_ = userPub

	// Enrol a device under it, by its user-signed cert.
	cert, err := enrolment.SignCert(userPriv, devicePub, "", fixedTime, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp = h.do(http.MethodPost, "/api/v1/identities/devices", admin.Secret,
		strings.NewReader(fmt.Sprintf(`{"cert":%q,"name":"phone"}`, cert)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("enrol device: status = %d, want 201 (body: %s)", resp.StatusCode, h.body(resp))
	}
	var device struct {
		DeviceKey   string `json:"device_key"`
		AdmittedBy  string `json:"admitted_by"`
		AdmittingOp string `json:"admitting_op"`
	}
	if err := json.Unmarshal(h.body(resp), &device); err != nil {
		t.Fatal(err)
	}
	if device.DeviceKey != deviceKey {
		t.Fatalf("enrolled device key = %q, want %q", device.DeviceKey, deviceKey)
	}
	// Provenance (ADR-0068): a cert-era admission is by the genesis key.
	if device.AdmittedBy != userKey || device.AdmittingOp != enrolment.OpHash(cert) {
		t.Fatalf("admitted_by = %q admitting_op = %q, want %q / %q", device.AdmittedBy, device.AdmittingOp, userKey, enrolment.OpHash(cert))
	}

	// The device is listed under its user.
	resp = h.do(http.MethodGet, "/api/v1/identities/users/"+userKey+"/devices", admin.Secret, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list devices: status = %d, want 200", resp.StatusCode)
	}
	var devices struct {
		Items []struct {
			DeviceKey string `json:"device_key"`
		} `json:"items"`
	}
	if err := json.Unmarshal(h.body(resp), &devices); err != nil {
		t.Fatal(err)
	}
	if len(devices.Items) != 1 || devices.Items[0].DeviceKey != deviceKey {
		t.Fatalf("listed devices = %+v, want one device %q", devices.Items, deviceKey)
	}

	// Revoke the device: a tombstone, so revoked_at is set.
	resp = h.do(http.MethodDelete, "/api/v1/identities/devices/"+deviceKey, admin.Secret, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke device: status = %d, want 200", resp.StatusCode)
	}
	var revoked struct {
		RevokedAt *string `json:"revoked_at"`
	}
	if err := json.Unmarshal(h.body(resp), &revoked); err != nil {
		t.Fatal(err)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("revoked device has a null revoked_at")
	}

	// Revoke the user: 200, and it is gone from the list.
	if resp := h.do(http.MethodDelete, "/api/v1/identities/users/"+userKey, admin.Secret, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke user: status = %d, want 200", resp.StatusCode)
	}
	resp = h.do(http.MethodGet, "/api/v1/identities/users", admin.Secret, nil)
	var users struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(h.body(resp), &users); err != nil {
		t.Fatal(err)
	}
	if len(users.Items) != 0 {
		t.Fatalf("after revoke, %d users remain, want 0", len(users.Items))
	}
}

// TestEnrolDeviceRefusesAnUnpinnedUser is the enrol-before-trust gate at the
// API: a cert signed by a user the node never pinned is a 404, not a silent
// enrolment.
func TestEnrolDeviceRefusesAnUnpinnedUser(t *testing.T) {
	h := newHarness(t, withAuth)
	admin := h.mint("admin", auth.ScopeAdmin)

	_, userPriv, _ := newIdentityKey(t) // never pinned
	devicePub, _, _ := newIdentityKey(t)
	cert, err := enrolment.SignCert(userPriv, devicePub, "", fixedTime, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp := h.do(http.MethodPost, "/api/v1/identities/devices", admin.Secret,
		strings.NewReader(fmt.Sprintf(`{"cert":%q}`, cert)))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("enrol device for an unpinned user: status = %d, want 404", resp.StatusCode)
	}
}

// TestEnrolUserRejectsAMalformedKey: a public key that is not a rendered
// Ed25519 key is a 400 paste error, not a 500.
func TestEnrolUserRejectsAMalformedKey(t *testing.T) {
	h := newHarness(t, withAuth)
	admin := h.mint("admin", auth.ScopeAdmin)
	resp := h.do(http.MethodPost, "/api/v1/identities/users", admin.Secret,
		strings.NewReader(`{"public_key":"not-a-key"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("enrol user with a malformed key: status = %d, want 400", resp.StatusCode)
	}
}
