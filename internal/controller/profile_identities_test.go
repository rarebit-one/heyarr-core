package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/enrolment"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// The personal profile serves the device tombstone (#914).
//
// `mnemosyne device revoke` answered "no route matches
// /api/v1/identities/devices/..." against a live node: the identity admin
// surface was registered only by the resource API's full Mount, which the
// personal profile does not call. As with /peers (#762), this drives the route
// on the real personal-profile router with authentication on, by every name
// an operator has for a device, and checks that only the tombstone came along.

// wireIdentityDevice is the subset of the IdentityDevice wire shape these
// assertions read.
type wireIdentityDevice struct {
	ID            string  `json:"id"`
	DeviceKey     string  `json:"device_key"`
	EncryptionKey string  `json:"encryption_key"`
	RevokedAt     *string `json:"revoked_at"`
}

func TestPersonalProfileRevokesADevice(t *testing.T) {
	ctx := context.Background()
	c := newProfileHandler(t, config.MnemosyneDefaults())
	h := newProfileSurfaceHarness(t, c, noPeerPresented)
	admin := h.token(t, "admin", auth.ScopeAdmin)
	writer := h.token(t, "writer", auth.ScopeWrite)

	// A pinned user with three devices, the way `admin user pin` and POST
	// /enrol leave them on a Mnemosyne node.
	u, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.identities.EnrolUser(ctx, u.UserID(), "owner", ""); err != nil {
		t.Fatal(err)
	}
	enrolDevice := func(name string) (key, id string) {
		t.Helper()
		pub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := enrolment.SignCert(userPriv, pub, "x25519:"+strings.Repeat("ab", 32), time.Now(), 0)
		if err != nil {
			t.Fatal(err)
		}
		d, err := h.identities.EnrolDevice(ctx, cert, name)
		if err != nil {
			t.Fatal(err)
		}
		return identity.FormatPublicKey(pub), d.ID
	}
	devices := httpapi.APIPrefix + "/identities/devices/"

	rendered, _ := enrolDevice("old-phone")
	bareKey, _ := enrolDevice("old-tablet")
	byIDKey, byID := enrolDevice("old-laptop")
	for _, tc := range []struct{ name, ref, wantKey string }{
		{"rendered key", rendered, rendered},
		{"bare hex key", strings.TrimPrefix(bareKey, identity.Algorithm+":"), bareKey},
		{"device identity id", byID, byIDKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Admin only: ending a device's trust is a trust decision.
			if status, raw, _ := call(t, h, http.MethodDelete, devices+tc.ref, writer, nil); status != http.StatusForbidden {
				t.Fatalf("DELETE with a write token answered %d, want 403\n%s", status, raw)
			}
			status, raw, _ := call(t, h, http.MethodDelete, devices+tc.ref, admin, nil)
			if status != http.StatusOK {
				t.Fatalf("DELETE %s%s on the personal profile answered %d, want 200\n%s", devices, tc.ref, status, raw)
			}
			var got wireIdentityDevice
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			// The record carries the encryption key `device revoke` re-keys
			// spaces away from.
			if got.DeviceKey != tc.wantKey || got.RevokedAt == nil || got.EncryptionKey == "" {
				t.Fatalf("revoked = %s", raw)
			}
			d, err := h.identities.LookupDevice(ctx, tc.wantKey)
			if err != nil || d.RevokedAt == nil {
				t.Fatalf("the device is not tombstoned in the store: %+v, %v", d, err)
			}
			// Idempotent.
			if status, raw, _ := call(t, h, http.MethodDelete, devices+tc.ref, admin, nil); status != http.StatusOK {
				t.Fatalf("a second revoke answered %d, want 200\n%s", status, raw)
			}
		})
	}

	// An unknown device is a 404 from the handler, not the router's "no route".
	status, raw, _ := call(t, h, http.MethodDelete, devices+"ed25519:"+strings.Repeat("00", 32), admin, nil)
	if status != http.StatusNotFound || !strings.Contains(string(raw), "not enrolled") {
		t.Fatalf("revoking an unknown device answered %d, want the store's 404\n%s", status, raw)
	}

	// The space listing `device revoke` sweeps after the tombstone answers
	// for the same admin token.
	if status, raw, _ := call(t, h, http.MethodGet, httpapi.APIPrefix+"/spaces", admin, nil); status != http.StatusOK {
		t.Fatalf("GET /spaces with the admin token answered %d, want 200\n%s", status, raw)
	}

	// Nothing else from the identity admin surface came along: users are still
	// pinned from the host, and devices enrol through POST /enrol.
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/identities/users"},
		{http.MethodPost, "/identities/users"},
		{http.MethodDelete, "/identities/users/" + u.UserID()},
		{http.MethodGet, "/identities/users/" + u.UserID() + "/devices"},
		{http.MethodPost, "/identities/devices"},
	} {
		status, raw, _ := call(t, h, probe.method, httpapi.APIPrefix+probe.path, admin, map[string]string{})
		if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			t.Errorf("%s %s%s on the personal profile answered %d, want it unmounted\n%s",
				probe.method, httpapi.APIPrefix, probe.path, status, raw)
		}
	}
}
