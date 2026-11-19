package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/device"
	"github.com/rarebit-one/void-which-binds-go/enrolment"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// ownDevice is another device of an enrolled user: its key directory and the
// device identity the controller pinned for it.
type ownDevice struct {
	dir    string
	encKey string
	pinned deviceauth.Device
}

// enrolAnotherDevice enrols one more software device for u, signed by u's
// user key — what pairing a new phone leaves behind: a device of the same
// identity that holds no space key yet.
func (h *apiHarness) enrolAnotherDevice(t *testing.T, u householdUser, name string) ownDevice {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	ds, err := device.NewStore(device.StoreOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := ds.Generate(name, false)
	if err != nil {
		t.Fatal(err)
	}
	// Valid on both the harness's fixed clock and the wall clock, as in enrolUser.
	lifetime := time.Since(h.clock.Now()) + enrolment.CertLifetime
	cert, err := enrolment.SignCert(u.userPriv, dev.PublicKey, dev.EncryptionKeyString(), h.clock.Now(), lifetime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ds.Enrol(cert); err != nil {
		t.Fatal(err)
	}
	pinned, err := h.identities.EnrolDevice(context.Background(), cert, name)
	if err != nil {
		t.Fatal(err)
	}
	return ownDevice{dir: dir, encKey: dev.EncryptionKeyString(), pinned: pinned}
}

// TestSpaceWrapGivesANewDeviceItsSpaces drives `space wrap` through the real
// command tree: alice's laptop creates a personal space, so only it holds the
// key; she then enrols a phone, which cannot open the space. A dry run names
// the phone and wraps nothing; the real run wraps the key for it and the phone
// opens the space; a second run finds nothing to do. Another user's device, a
// revoked device of hers, and a missing or doubled selector are refused.
func TestSpaceWrapGivesANewDeviceItsSpaces(t *testing.T) {
	ctx := context.Background()
	h := newAPIHarness(t, withAPIAuth, withWrapAuthorizer, withDeviceCredentials)
	token, err := h.tokens.Create(ctx, "cli", []auth.Scope{auth.ScopeRead, auth.ScopeWrite, auth.ScopeAdmin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	alice, bob := h.enrolUser(t, "alice"), h.enrolUser(t, "bob")

	var created spaceCreateView
	out := h.mustRun("space", "create", "--kind", "personal", "--recovery=false", "--device-dir", alice.dir,
		"--token", token.Secret, "--json")
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("space create --json: %v\n%s", err, out)
	}
	spaceID := created.ID

	phone := h.enrolAnotherDevice(t, alice, "alice-phone")
	if _, _, err := h.run("space", "read", spaceID, "--device-dir", phone.dir, "--token", token.Secret); err == nil {
		t.Fatal("the new phone read the space before any copy was wrapped for it")
	}

	wrap := func(args ...string) spaceWrapView {
		t.Helper()
		out := h.mustRun(append([]string{"space", "wrap", spaceID, "--device-dir", alice.dir, "--json"}, args...)...)
		var v spaceWrapView
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatalf("space wrap --json: %v\n%s", err, out)
		}
		return v
	}

	// A dry run names the phone and wraps nothing.
	if v := wrap("--all-devices", "--dry-run"); !v.DryRun || len(v.Wrapped) != 1 || v.Wrapped[0].ID != phone.pinned.ID {
		t.Fatalf("dry run = %+v, want the phone", v)
	}
	if _, _, err := h.run("space", "read", spaceID, "--device-dir", phone.dir, "--token", token.Secret); err == nil {
		t.Fatal("a dry run wrapped the key")
	}

	out = h.mustRun("space", "wrap", spaceID, "--all-devices", "--device-dir", alice.dir, "--json")
	var v spaceWrapView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("space wrap --json: %v\n%s", err, out)
	}
	if v.DryRun || v.KeyEpoch != 0 || len(v.Wrapped) != 1 || v.Wrapped[0].Recipient != phone.encKey || len(v.AlreadyHeld) != 0 {
		t.Fatalf("wrap = %+v, want the phone wrapped for", v)
	}
	normalised := strings.NewReplacer(spaceID, "<space>", phone.pinned.ID, "<phone>", phone.encKey, "<phone-key>").Replace(out)
	testutil.Golden(t, "testdata/space_wrap.json", []byte(normalised))

	// The phone now opens the space.
	var read spaceReadView
	out = h.mustRun("space", "read", spaceID, "--device-dir", phone.dir, "--token", token.Secret, "--json")
	if err := json.Unmarshal([]byte(out), &read); err != nil || read.SpaceID != spaceID {
		t.Fatalf("the phone reads the space: %v\n%s", err, out)
	}

	// Running it again does nothing, and naming the phone reports it as held.
	if v := wrap("--all-devices"); len(v.Wrapped) != 0 || len(v.AlreadyHeld) != 1 {
		t.Fatalf("a second run = %+v, want nothing wrapped and the phone already holding it", v)
	}
	if v := wrap("--device", phone.pinned.ID, "--device", phone.encKey); len(v.Wrapped) != 0 || len(v.AlreadyHeld) != 1 {
		t.Fatalf("naming the phone twice = %+v", v)
	}
	human := h.mustRun("space", "wrap", spaceID, "--all-devices", "--device-dir", alice.dir)
	if !strings.Contains(human, "nothing to wrap") || !strings.Contains(human, phone.pinned.ID) {
		t.Fatalf("human output:\n%s", human)
	}

	// Refusals: another user's device, a revoked device of hers, no selector,
	// both selectors.
	old := h.enrolAnotherDevice(t, alice, "alice-old-phone")
	if _, err := h.identities.RevokeDevice(ctx, old.pinned.DeviceKey); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"another user's device", []string{"--device", bob.encKey}, "not one of your devices"},
		{"a revoked device", []string{"--device", old.pinned.ID}, "revoked"},
		{"no selector", nil, "--all-devices"},
		{"both selectors", []string{"--all-devices", "--device", phone.pinned.ID}, "not both"},
	} {
		args := append([]string{"space", "wrap", spaceID, "--device-dir", alice.dir}, tc.args...)
		if _, stderr, err := h.run(args...); err == nil || !strings.Contains(err.Error()+stderr, tc.want) {
			t.Errorf("%s: %v\n%s; want an error naming %q", tc.name, err, stderr, tc.want)
		}
	}
}
