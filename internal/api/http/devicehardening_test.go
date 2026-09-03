// The responses `probe` returns are closed by the t.Cleanup it registers, which
// bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package httpapi_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
)

// Device-scheme hardening (#420), found by the real mobile client: the 401 says
// nothing at all, the proof's TTL was whatever the device signed, and a session
// never re-checked the device that approved it.

// The two CLOCK refusals carry a hint, because a caller already knows the window
// it signed and learns nothing about identity from being told the server
// disagrees. The body stays opaque either way.
func TestClockWindowRefusalsCarryAHint(t *testing.T) {
	t.Parallel()
	h := newDeviceAuthHarness(t)
	if _, credential := h.enrolledDevice(t); credential() == "" {
		t.Fatal("no credential")
	}

	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{"a proof from long enough ago", time.Now().UTC().Add(-time.Hour), "expired"},
		{"a proof from a device whose clock runs fast", time.Now().UTC().Add(time.Hour), "not_yet_valid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.probe(t, "Device "+h.credentialAt(t, tt.at, 0))
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); got != `Device error="`+tt.want+`"` {
				t.Errorf("WWW-Authenticate = %q, want the %s hint", got, tt.want)
			}
			// The body discloses nothing more than it ever did.
			body, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(body), tt.want) {
				t.Errorf("the body leaked the reason: %s", body)
			}
		})
	}
}

// Every OTHER refusal stays undifferentiated. Telling a caller that the device
// is revoked, or that the cert is for someone else, is the free reconnaissance
// the opaque 401 exists to withhold.
func TestNonClockRefusalsCarryNoHint(t *testing.T) {
	t.Parallel()
	h := newDeviceAuthHarness(t)
	deviceKey, credential := h.enrolledDevice(t)

	if _, err := h.store.RevokeDevice(context.Background(), deviceKey); err != nil {
		t.Fatal(err)
	}
	resp := h.probe(t, "Device "+credential())
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("a revoked device was told %q; that is reconnaissance", got)
	}
}

// A proof may not grant itself more than MaxPossessionTTL, however long the
// device signed for — the proof's window IS its replay window.
func TestPossessionTTLIsCappedServerSide(t *testing.T) {
	t.Parallel()
	h := newDeviceAuthHarness(t)
	h.enrolledDevice(t)
	now := time.Now().UTC()

	// At the cap: honoured, so the ceiling is usable rather than nominal.
	if code := h.get(t, "Device "+h.credentialAt(t, now, deviceauth.MaxPossessionTTL)); code != http.StatusOK {
		t.Errorf("a proof at exactly the cap = %d, want 200", code)
	}
	// Over it: refused, even though it is neither expired nor not-yet-valid.
	resp := h.probe(t, "Device "+h.credentialAt(t, now, deviceauth.MaxPossessionTTL+time.Minute))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an over-long proof = %d, want 401", resp.StatusCode)
	}
	// It is a policy refusal, not a clock one, so it gets no re-mint hint: a
	// client that re-minted the same over-long proof would loop forever.
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("an over-long proof was hinted %q, which would be retried forever", got)
	}
}

// The TTL cap is judged on the window the proof CLAIMED, not on the time left in
// it: an hour-long proof looks perfectly ordinary nine minutes before it expires.
func TestPossessionTTLCapReadsTheClaimedWindowNotTheRemainder(t *testing.T) {
	t.Parallel()
	h := newDeviceAuthHarness(t)
	h.enrolledDevice(t)

	// Signed an hour ago for an hour: a minute of it is left, which is well
	// inside the cap, and it must still be refused.
	over := h.credentialAt(t, time.Now().UTC().Add(-59*time.Minute), time.Hour)
	if code := h.get(t, "Device "+over); code != http.StatusUnauthorized {
		t.Errorf("an hour-long proof with a minute left = %d, want 401", code)
	}
}

// stubMembers is a DeviceMembership over a fixed set of live device keys.
type stubMembers map[string]bool

func (m stubMembers) DeviceActive(_ context.Context, deviceKey string) error {
	if m[deviceKey] {
		return nil
	}
	return deviceauth.ErrDeviceRevoked
}

// A web-login session pins the device that approved it (ADR-0053), and that pin
// is now re-read on every request: revoking the approver stops its sessions on
// the next request rather than at their own expiry.
func TestSessionIsRefusedWhenItsApprovingDeviceIsNoLongerAMember(t *testing.T) {
	t.Parallel()
	const live, revoked = "session-live", "session-revoked"
	sessions := stubSessions{
		live:    {UserID: "ed25519:aa", DeviceKey: "ed25519:approver"},
		revoked: {UserID: "ed25519:aa", DeviceKey: "ed25519:gone"},
	}
	ts := newSessionHarnessWithMembership(t, sessions, stubMembers{"ed25519:approver": true})

	if code := getProbe(t, ts, "Bearer "+live); code != http.StatusOK {
		t.Errorf("a session whose approver is still enrolled = %d, want 200", code)
	}
	if code := getProbe(t, ts, "Bearer "+revoked); code != http.StatusUnauthorized {
		t.Errorf("a session whose approver was revoked = %d, want 401", code)
	}
}

// A session principal that names no approving device cannot be checked against
// membership at all, and "I cannot check" must not resolve in the caller's
// favour.
func TestSessionWithNoApprovingDeviceIsRefusedWhenMembershipIsChecked(t *testing.T) {
	t.Parallel()
	const token = "session-anonymous"
	sessions := stubSessions{token: {UserID: "ed25519:aa"}}
	ts := newSessionHarnessWithMembership(t, sessions, stubMembers{})

	if code := getProbe(t, ts, "Bearer "+token); code != http.StatusUnauthorized {
		t.Errorf("a session naming no approver = %d, want 401", code)
	}
}

// With no membership checker wired the session path is exactly what it was —
// the state of every deployment that predates this, and one where there is no
// store to revoke in.
func TestSessionIsUnchangedWithNoMembershipChecker(t *testing.T) {
	t.Parallel()
	const token = "session-token-abc"
	ts := newSessionHarness(t, stubSessions{token: {UserID: "ed25519:aa", DeviceKey: "ed25519:bb"}})

	if code := getProbe(t, ts, "Bearer "+token); code != http.StatusOK {
		t.Errorf("a session with no membership checker = %d, want 200", code)
	}
}

// The store answers the membership question the session path asks, and answers
// it the way the verifier already does.
func TestDeviceActiveTracksEnrolmentAndRevocation(t *testing.T) {
	t.Parallel()
	h := newDeviceAuthHarness(t)
	deviceKey, _ := h.enrolledDevice(t)
	ctx := context.Background()

	if err := h.store.DeviceActive(ctx, deviceKey); err != nil {
		t.Errorf("an enrolled device is not active: %v", err)
	}
	if err := h.store.DeviceActive(ctx, "ed25519:"+strings.Repeat("00", ed25519.PublicKeySize)); !errors.Is(err, deviceauth.ErrUnknownDevice) {
		t.Errorf("an unenrolled key = %v, want ErrUnknownDevice", err)
	}
	if _, err := h.store.RevokeDevice(ctx, deviceKey); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DeviceActive(ctx, deviceKey); !errors.Is(err, deviceauth.ErrDeviceRevoked) {
		t.Errorf("a revoked device = %v, want ErrDeviceRevoked", err)
	}
}

// A proof whose window ends before it starts is malformed, not "well inside the
// cap": VerifyPossession has no opinion about the ordering, so the cap must.
func TestPossessionWindowThatEndsBeforeItStartsIsRefused(t *testing.T) {
	t.Parallel()
	h := newDeviceAuthHarness(t)
	h.enrolledDevice(t)

	// SignPossession will not make one, so it is assembled by hand from the
	// same parts: a body the device really signed, with exp before iat.
	proof := signedPossessionWindow(t, h.devicePriv, h.deviceCert,
		time.Now().UTC(), time.Now().UTC().Add(-time.Minute))
	if code := h.get(t, "Device "+h.deviceCert+"~"+proof); code != http.StatusUnauthorized {
		t.Errorf("an inverted window = %d, want 401", code)
	}
}

// signedPossessionWindow assembles a possession proof by hand, so a test can
// give it a window SignPossession would refuse to make. The body is the same
// shape voidbind-go signs — version, cert hash, iat, exp — and it is really
// signed by the device key, so everything up to the window check passes and the
// window check is what the assertion is about.
func signedPossessionWindow(t *testing.T, priv ed25519.PrivateKey, cert string, issued, expires time.Time) string {
	t.Helper()
	sum := sha256.Sum256([]byte(cert))
	body, err := json.Marshal(map[string]any{
		"v":   enrolment.Version,
		"crt": base64.RawURLEncoding.EncodeToString(sum[:]),
		"iat": issued.Unix(),
		"exp": expires.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(body) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, body))
}
