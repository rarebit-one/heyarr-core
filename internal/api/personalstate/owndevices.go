package personalstate

// owndevices.go answers "which of my devices hold this space's key?" for a
// device of the space's own user (ADR-0049's re-wrap for a newly enrolled
// device). Pairing a new device admits it to the identity but wraps it no
// space key, and the server cannot wrap one (Invariant 6). So a device that
// holds the key asks here which of its user's other enrolled devices lack a
// copy at the current epoch, wraps the key for them itself, and pushes the
// copies through POST /spaces/{id}/keys. Enrol-before-wrap and the membership
// gate still decide there whether each copy is admitted.

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
)

// The statuses of an own device: whether a space key may be wrapped for it.
const (
	// OwnDeviceActive is an enrolled, unrevoked device with an encryption key:
	// a wrap target.
	OwnDeviceActive = "active"
	// OwnDeviceRevoked is a revoked device, which enrol-before-wrap refuses.
	OwnDeviceRevoked = "revoked"
	// OwnDeviceNoEncryptionKey is a device enrolled by a cert that binds no
	// encryption key (v1), so there is nothing to wrap for.
	OwnDeviceNoEncryptionKey = "no_encryption_key"
)

// ownDeviceView is one of the caller's user's devices, as a wrap target.
type ownDeviceView struct {
	// ID is the device identity's id, the one pairing reports.
	ID        string `json:"id"`
	Name      string `json:"name"`
	DeviceKey string `json:"device_key"`
	// Recipient is the device's encryption key ("x25519:<hex>"), the key a
	// space key is wrapped for; empty for a device with none.
	Recipient string `json:"recipient,omitempty"`
	Status    string `json:"status"`
	// HoldsCurrentKey is true when a copy of the space's current key is
	// wrapped for Recipient.
	HoldsCurrentKey bool `json:"holds_current_key"`
	// ThisDevice marks the caller's own device.
	ThisDevice bool   `json:"this_device"`
	EnrolledAt string `json:"enrolled_at"`
}

// ownDevicesView is GET /spaces/{id}/own-devices.
type ownDevicesView struct {
	SpaceID string `json:"space_id"`
	// KeyEpoch is the space's current key epoch (ADR-0103), the one a wrap
	// for a device lacking a copy must seal.
	KeyEpoch int `json:"key_epoch"`
	// MemberWrite is true when the calling device may push changes to this
	// space with its own credential as a writing member (ADR-0128). A node
	// that predates ADR-0128 omits it, which is how a client knows to keep
	// using a write token there.
	MemberWrite bool            `json:"member_write"`
	Devices     []ownDeviceView `json:"devices"`
}

func (a *API) listOwnDevices(w http.ResponseWriter, r *http.Request) {
	id, ok := httpapi.IdentityFrom(r.Context())
	if !ok || id.DeviceKey == "" || id.Principal.ID == "" {
		httpapi.Fail(w, r, problem.Forbidden(
			"a space's own devices are listed for a Device credential, whose user they are; a bearer token or session has none"))
		return
	}
	if !a.membersWired(w, r) {
		return
	}
	spaceID := chi.URLParam(r, "id")
	// One read, so the epoch and the copies describe the same moment.
	epoch, keys, err := a.store.KeyState(r.Context(), spaceID)
	if err != nil {
		a.failStore(w, r, "reading the key state", err)
		return
	}
	devices, err := a.members.PrincipalDevices(r.Context(), id.Principal.ID)
	if err != nil {
		a.log.Error("listing the caller's devices", "request_id", httpapi.RequestIDFrom(r.Context()), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	var memberWrite bool
	if a.memberW != nil {
		if memberWrite, err = a.memberW.MemberMayWrite(r.Context(), id.Principal.ID, id.DeviceKey, spaceID); err != nil {
			a.log.Error("checking member write", "request_id", httpapi.RequestIDFrom(r.Context()), "error", err)
			httpapi.Fail(w, r, problem.Internal())
			return
		}
	}
	held := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k.Epoch == epoch {
			held[k.Recipient] = true
		}
	}
	out := ownDevicesView{SpaceID: spaceID, KeyEpoch: epoch, MemberWrite: memberWrite, Devices: make([]ownDeviceView, 0, len(devices))}
	for _, d := range devices {
		out.Devices = append(out.Devices, ownDeviceView{
			ID: d.ID, Name: d.Name, DeviceKey: d.DeviceKey, Recipient: d.EncryptionKey,
			Status:          ownDeviceStatus(d),
			HoldsCurrentKey: d.EncryptionKey != "" && held[d.EncryptionKey],
			ThisDevice:      d.DeviceKey == id.DeviceKey,
			EnrolledAt:      d.EnrolledAt.UTC().Format(timeFormat),
		})
	}
	a.write(w, r, http.StatusOK, out)
}

func ownDeviceStatus(d deviceauth.Device) string {
	switch {
	case d.RevokedAt != nil:
		return OwnDeviceRevoked
	case d.EncryptionKey == "":
		return OwnDeviceNoEncryptionKey
	default:
		return OwnDeviceActive
	}
}
