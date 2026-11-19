package personalstate

import (
	"net/http"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

const ownSpace = "0199a0a0-0000-7000-8000-000000000049"

// TestOwnDevicesNamesTheCopiesAUsersDevicesLack is the newly-enrolled-device
// scene (ADR-0049): bob's first device creates a personal space, so only it
// and the recovery key hold the key. Listing his own devices from it names his
// second device as active but lacking a copy, and his revoked and keyless
// devices as not wrap targets. His first device wraps for the second through
// /keys — admitted on the owned (membership-gated) space, since it is the
// owner's own device — and the second then shows as holding the current key.
func TestOwnDevicesNamesTheCopiesAUsersDevicesLack(t *testing.T) {
	t.Parallel()
	revokedAt := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	members := household()
	members.extra = map[string][]deviceauth.Device{"p-bob": {
		{ID: "d-old", Name: "old phone", DeviceKey: "ed25519:old", EncryptionKey: "x25519:dddd111111111111111111111111111111111111111111111111111111111111", EnrolledAt: revokedAt.AddDate(0, -6, 0), RevokedAt: &revokedAt},
		{ID: "d-v1", Name: "v1 device", DeviceKey: "ed25519:v1", EnrolledAt: revokedAt.AddDate(-1, 0, 0)},
	}}
	api := membersAPI(t, members)
	bob := auth.Identity{DeviceKey: "ed25519:" + bobKey1[7:15], Principal: auth.Principal{ID: "p-bob", Kind: auth.KindUser, Name: "bob"}}
	createAs(t, api, bob, ownSpace, "personal", bobKey1, recoveryKey)

	list := func() ownDevicesView {
		t.Helper()
		rec := as(t, bob, api.listOwnDevices, http.MethodGet, "/d", nil, spaceParams(ownSpace))
		if rec.Code != http.StatusOK {
			t.Fatalf("own devices: %d %s", rec.Code, rec.Body)
		}
		var v ownDevicesView
		mustJSON(t, rec, &v)
		return v
	}
	rec := as(t, bob, api.listOwnDevices, http.MethodGet, "/d", nil, spaceParams(ownSpace))
	if rec.Code != http.StatusOK {
		t.Fatalf("own devices: %d %s", rec.Code, rec.Body)
	}
	testutil.Golden(t, "testdata/own_devices.json", indent(t, rec.Body.Bytes()))

	byID := map[string]ownDeviceView{}
	for _, d := range list().Devices {
		byID[d.ID] = d
	}
	if d := byID["d-bbbb1111"]; !d.ThisDevice || !d.HoldsCurrentKey || d.Status != OwnDeviceActive {
		t.Errorf("the calling device = %+v, want this device, holding the key", d)
	}
	if d := byID["d-bbbb2222"]; d.ThisDevice || d.HoldsCurrentKey || d.Status != OwnDeviceActive || d.Recipient != bobKey2 {
		t.Errorf("the new device = %+v, want an active device lacking a copy", d)
	}
	if byID["d-old"].Status != OwnDeviceRevoked || byID["d-v1"].Status != OwnDeviceNoEncryptionKey || len(byID) != 4 {
		t.Errorf("devices = %+v", byID)
	}

	// The owner's own new device is a current member's device, so the gate an
	// owned space carries admits the wrap for it.
	wantCode(t, rewrap(t, api, ownSpace, bobKey2, 0), http.StatusOK, "")
	for _, d := range list().Devices {
		if d.ID == "d-bbbb2222" && !d.HoldsCurrentKey {
			t.Errorf("after the wrap, the new device = %+v, want it holding the key", d)
		}
	}

	// Only a Device credential has own devices.
	wantCode(t, as(t, bearer, api.listOwnDevices, http.MethodGet, "/d", nil, spaceParams(ownSpace)), http.StatusForbidden, "")
	wantCode(t, as(t, bob, api.listOwnDevices, http.MethodGet, "/d", nil, spaceParams("0199a0a0-0000-7000-8000-00000000dead")),
		http.StatusNotFound, "")
	// With no resolver wired the route is unavailable, not empty.
	wantCode(t, as(t, bob, membersAPI(t, nil).listOwnDevices, http.MethodGet, "/d", nil, spaceParams(ownSpace)),
		http.StatusServiceUnavailable, "")
}
