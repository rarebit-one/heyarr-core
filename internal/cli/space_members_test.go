package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/device"
	"github.com/rarebit-one/void-which-binds-go/enrolment"

	psapi "github.com/rarebit-one/heyarr-core/internal/api/personalstate"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// household is a user with one enrolled, management-authorised device whose
// key lives in dir — enough to present a Device credential to the harness.
type householdUser struct {
	name, principal, encKey, dir string
	store                        *device.Store
}

// enrolUser pins a fresh user identity on the harness and enrols one software
// device for it, so the CLI run with --device-dir dir authenticates as them.
func (h *apiHarness) enrolUser(t *testing.T, name string) householdUser {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), name)
	ds, err := device.NewStore(device.StoreOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := ds.Generate(name+"-laptop", false)
	if err != nil {
		t.Fatal(err)
	}
	user, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := enrolment.SignCert(userPriv, dev.PublicKey, dev.EncryptionKeyString(), h.clock.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ds.Enrol(cert); err != nil {
		t.Fatal(err)
	}
	u, err := h.identities.EnrolUser(ctx, user.UserID(), name, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.identities.EnrolDevice(ctx, cert, name+"-laptop"); err != nil {
		t.Fatal(err)
	}
	return householdUser{name: name, principal: u.PrincipalID, encKey: dev.EncryptionKeyString(), dir: dir, store: ds}
}

// TestSpaceMembersShareAndUnshare drives `space members add|list|remove` through
// the real command tree against a controller that authenticates Device
// credentials (ADR-0108): the owner's device shares a space with bob, bob's
// device can then open it, the list shows him, the removal names the device
// key the rotation must revoke, and after it no copy may be wrapped for him.
// A non-owner's device cannot add anyone to an owned space.
func TestSpaceMembersShareAndUnshare(t *testing.T) {
	ctx := context.Background()
	h := newAPIHarness(t, withAPIAuth, withWrapAuthorizer, withDeviceCredentials)
	token, err := h.tokens.Create(ctx, "cli", []auth.Scope{auth.ScopeRead, auth.ScopeWrite, auth.ScopeAdmin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	alice, bob := h.enrolUser(t, "alice"), h.enrolUser(t, "bob")

	// The space, created by alice's device with a bearer: it is wrapped for her
	// device and, since a bearer names no owner, it records none.
	var created spaceCreateView
	out := h.mustRun("space", "create", "--kind", "shared", "--recovery=false", "--device-dir", alice.dir,
		"--token", token.Secret, "--json")
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("space create --json: %v\n%s", err, out)
	}
	spaceID := created.ID

	// Bob cannot read it yet: no copy of the key is wrapped for his device.
	if _, _, err := h.run("space", "read", spaceID, "--device-dir", bob.dir, "--token", token.Secret); err == nil {
		t.Fatal("bob read the space before being a member")
	}

	// Alice's device shares it: the membership is recorded and the key is
	// wrapped for bob's enrolled device.
	var added spaceMemberAddedView
	out = h.mustRun("space", "members", "add", spaceID, "--user", "bob", "--device-dir", alice.dir, "--json")
	if err := json.Unmarshal([]byte(out), &added); err != nil {
		t.Fatalf("members add --json: %v\n%s", err, out)
	}
	if added.PrincipalID != bob.principal || added.Role != "member" || added.KeyEpoch != 0 ||
		len(added.WrappedFor) != 1 || added.WrappedFor[0] != bob.encKey {
		t.Fatalf("added = %+v, want bob as member with his device wrapped for", added)
	}
	testutil.Golden(t, "testdata/space_members_add.json", []byte(normaliseMembers(out, spaceID, bob)))

	// Now bob's device opens the space.
	var read spaceReadView
	out = h.mustRun("space", "read", spaceID, "--device-dir", bob.dir, "--token", token.Secret, "--json")
	if err := json.Unmarshal([]byte(out), &read); err != nil || read.SpaceID != spaceID {
		t.Fatalf("bob reads the space: %v\n%s", err, out)
	}

	// The list: bob, a member; no owner row, the space having none.
	var members []apiclient.SpaceMember
	out = h.mustRun("space", "members", "list", spaceID, "--token", token.Secret, "--json")
	if err := json.Unmarshal([]byte(out), &members); err != nil {
		t.Fatalf("members list --json: %v\n%s", err, out)
	}
	if len(members) != 1 || members[0].PrincipalID != bob.principal || members[0].Role != "member" || !members[0].Current() {
		t.Fatalf("members = %+v", members)
	}
	human := h.mustRun("space", "members", "list", spaceID, "--token", token.Secret)
	if !strings.Contains(human, bob.principal) || !strings.Contains(human, "member") {
		t.Fatalf("human list:\n%s", human)
	}

	// Removal: the tombstone, and the rotation that completes it names bob's
	// device, which holds the current key.
	var removed spaceMemberRemovedView
	out = h.mustRun("space", "members", "remove", spaceID, "--user", "bob", "--device-dir", alice.dir, "--json")
	if err := json.Unmarshal([]byte(out), &removed); err != nil {
		t.Fatalf("members remove --json: %v\n%s", err, out)
	}
	if removed.PrincipalID != bob.principal || removed.RemovedAt == "" ||
		len(removed.Revoke) != 1 || removed.Revoke[0] != bob.encKey ||
		removed.Rotation != "space rotate "+spaceID+" --revoke "+bob.encKey {
		t.Fatalf("removed = %+v", removed)
	}
	testutil.Golden(t, "testdata/space_members_remove.json", []byte(normaliseMembers(out, spaceID, bob)))
	out = h.mustRun("space", "members", "list", spaceID, "--token", token.Secret)
	if !strings.Contains(out, "removed ") {
		t.Fatalf("the list does not show the tombstone:\n%s", out)
	}
	// Bob still holds the key (nothing was rotated), but no new copy may be
	// wrapped for his device: a re-wrap is refused with the membership code.
	c, err := apiclient.New(apiclient.Options{Addr: h.server.URL, Token: token.Secret})
	if err != nil {
		t.Fatal(err)
	}
	err = c.RewrapKeys(ctx, spaceID, []apiclient.WrappedKeyInput{{Recipient: bob.encKey, Wrapped: []byte("opaque"), Epoch: 0}})
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) || apiErr.Problem == nil || apiErr.Problem.Code != psapi.CodeWrapRecipientNotMember {
		t.Fatalf("re-wrap for a removed member's device: %v", err)
	}
	if _, _, err := h.run("space", "members", "remove", spaceID, "--user", "bob", "--device-dir", alice.dir); err == nil {
		t.Fatal("removing bob twice succeeded")
	}
	// A user with no enrolled device cannot be added.
	if _, err := h.identities.EnrolUser(ctx, newUserKeyForTest(t), "carol", ""); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := h.run("space", "members", "add", spaceID, "--user", "carol", "--device-dir", alice.dir); err == nil ||
		!strings.Contains(err.Error()+stderr, "no enrolled device") {
		t.Fatalf("adding carol: %v\n%s", err, stderr)
	}

	// An OWNED space — created from alice's device credential — admits
	// membership changes from alice's device only.
	owned, err := apiclient.New(apiclient.Options{Addr: h.server.URL, Device: alice.store})
	if err != nil {
		t.Fatal(err)
	}
	sp, err := owned.CreateSpace(ctx, apiclient.CreateSpaceRequest{
		ID: "0199a0a0-0000-7000-8000-000000000755", Kind: string(spaces.KindPersonal),
		WrappedKeys: []apiclient.WrappedKeyInput{{Recipient: alice.encKey, Wrapped: []byte("opaque")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := h.run("space", "members", "add", sp.ID, "--user", "alice", "--device-dir", bob.dir); err == nil ||
		!strings.Contains(err.Error()+stderr, "owner") {
		t.Fatalf("bob's device changed an owned space's membership: %v\n%s", err, stderr)
	}
	out = h.mustRun("space", "members", "list", sp.ID, "--token", token.Secret, "--json")
	if err := json.Unmarshal([]byte(out), &members); err != nil || len(members) != 1 || members[0].Role != "owner" || members[0].PrincipalID != alice.principal {
		t.Fatalf("an owned space's members = %+v, %v; want alice as owner", members, err)
	}
}

// normaliseMembers replaces the run-specific values in a --json shape so the
// golden pins the structure: the space id, bob's principal and device key, and
// the timestamps the server stamped.
func normaliseMembers(out, spaceID string, bob householdUser) string {
	out = strings.ReplaceAll(out, spaceID, "<space>")
	out = strings.ReplaceAll(out, bob.principal, "<bob>")
	out = strings.ReplaceAll(out, bob.encKey, "<bob-device-key>")
	return timestampRE.ReplaceAllString(out, "<time>")
}

var timestampRE = regexp.MustCompile(`20\d\d-\d\d-\d\dT[0-9:.]+Z`)

// newUserKeyForTest is a fresh, well-formed user public key.
func newUserKeyForTest(t *testing.T) string {
	t.Helper()
	u, _, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return u.UserID()
}
