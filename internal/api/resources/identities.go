package resources

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
)

// The device-identity admin surface (§40, ADR-0048, ADR-0032).
//
// This is the operator-mediated pinning M8-02 left unbuilt: an operator pins a
// user's public key here, out of band, and enrols the device keys that user has
// vouched for. It is the ADR-0032 gate made reachable — "a key issued and
// immediately honoured must stay unspellable" — because nothing a user signs is
// honoured until the user is pinned through one of these calls.
//
// Every route is admin in both directions, and for the same reason peer
// enrolment is: pinning a user identity decides who may authenticate as a
// principal on this node, and a `write` token that could do it could mint
// itself an identity. The store emits the invariant-7 event for every
// transition inside its own transaction, so these handlers add none.

// identityUserFromStore renders a pinned user as the wire shape. It goes through
// the same struct the list endpoint scans into, so an enrolment response and a
// subsequent list cannot disagree about the same user.
func identityUserFromStore(u deviceauth.User) IdentityUser {
	return IdentityUser{
		ID:          u.ID,
		PrincipalID: u.PrincipalID,
		PublicKey:   u.PublicKey,
		Name:        u.Name,
		EnrolledAt:  u.EnrolledAt.UTC(),
	}
}

// identityDeviceFromStore renders an enrolled device as the wire shape, minus
// the cert (see IdentityDevice).
func identityDeviceFromStore(d deviceauth.Device) IdentityDevice {
	return IdentityDevice{
		ID:         d.ID,
		UserID:     d.UserID,
		DeviceKey:  d.DeviceKey,
		Name:       d.Name,
		EnrolledAt: d.EnrolledAt.UTC(),
		ExpiresAt:  d.ExpiresAt.UTC(),
		RevokedAt:  utcPtr(d.RevokedAt),
	}
}

// enrolUserRequest is the POST /identities/users body. The public key is the
// whole request: a user identity IS its key (ADR-0048), and a body without one
// would be a request to pin nothing.
type enrolUserRequest struct {
	PublicKey string `json:"public_key"`
	Name      string `json:"name"`
}

func (a *API) enrolUser(w http.ResponseWriter, r *http.Request) {
	var body enrolUserRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("public_key", body.PublicKey); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(
			err.Error()+"; a user identity is pinned by its public key (ADR-0048)"))
		return
	}
	user, err := a.identities.EnrolUser(r.Context(), body.PublicKey, body.Name)
	if err != nil {
		a.failIdentity(w, r, err)
		return
	}
	w.Header().Set("Location", httpapi.APIPrefix+"/identities/users/"+user.PublicKey)
	a.write(w, r, http.StatusCreated, identityUserFromStore(user))
}

func (a *API) listIdentityUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.identities.ListUsers(r.Context())
	if err != nil {
		a.fail(w, r, "identity user", err)
		return
	}
	out := make([]IdentityUser, 0, len(users))
	for _, u := range users {
		out = append(out, identityUserFromStore(u))
	}
	a.write(w, r, http.StatusOK, page[IdentityUser]{Items: out})
}

// revokeIdentityUser unpins a user, cascading to its devices (ADR-0012's shape,
// applied to a user). It returns the record it removed rather than 204, so an
// operator and the acceptance script can see exactly which key stopped being
// trusted.
func (a *API) revokeIdentityUser(w http.ResponseWriter, r *http.Request) {
	user, err := a.identities.RevokeUser(r.Context(), chi.URLParam(r, "key"))
	if err != nil {
		a.failIdentity(w, r, err)
		return
	}
	a.write(w, r, http.StatusOK, identityUserFromStore(user))
}

// listIdentityDevices lists one user's enrolled devices. It is scoped to a user
// rather than global because "which devices has this user enrolled?" is the
// question an operator asks before revoking one, and the user is how they name
// the set.
func (a *API) listIdentityDevices(w http.ResponseWriter, r *http.Request) {
	user, err := a.identities.LookupUser(r.Context(), chi.URLParam(r, "key"))
	if err != nil {
		a.failIdentity(w, r, err)
		return
	}
	devices, err := a.identities.ListDevices(r.Context(), user.ID)
	if err != nil {
		a.fail(w, r, "identity device", err)
		return
	}
	out := make([]IdentityDevice, 0, len(devices))
	for _, d := range devices {
		out = append(out, identityDeviceFromStore(d))
	}
	a.write(w, r, http.StatusOK, page[IdentityDevice]{Items: out})
}

// enrolDeviceRequest is the POST /identities/devices body. The cert is the whole
// request: it names its own user and device keys and is verified against the
// pinned user key before anything is written, so a device cannot be enrolled
// under a user who did not sign for it.
type enrolDeviceRequest struct {
	Cert string `json:"cert"`
	Name string `json:"name"`
}

func (a *API) enrolDevice(w http.ResponseWriter, r *http.Request) {
	var body enrolDeviceRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("cert", body.Cert); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(
			err.Error()+"; a device is enrolled by presenting its user-signed cert (ADR-0048)"))
		return
	}
	device, err := a.identities.EnrolDevice(r.Context(), body.Cert, body.Name)
	if err != nil {
		a.failIdentity(w, r, err)
		return
	}
	w.Header().Set("Location", httpapi.APIPrefix+"/identities/devices/"+device.DeviceKey)
	a.write(w, r, http.StatusCreated, identityDeviceFromStore(device))
}

// revokeIdentityDevice tombstones a single device. Unlike a user revoke it
// leaves the row, so a re-presented cert for the key is refused rather than
// silently re-enrolled.
func (a *API) revokeIdentityDevice(w http.ResponseWriter, r *http.Request) {
	device, err := a.identities.RevokeDevice(r.Context(), chi.URLParam(r, "key"))
	if err != nil {
		a.failIdentity(w, r, err)
		return
	}
	a.write(w, r, http.StatusOK, identityDeviceFromStore(device))
}

// failIdentity maps each store refusal to the status that tells a client what to
// do about it. Enumerated rather than collapsed into "400 on anything": a
// malformed key is a paste error, an already-enrolled key is a conflict, and an
// unknown one is a 404 — three different fixes.
func (a *API) failIdentity(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, deviceauth.ErrMalformedKey), errors.Is(err, deviceauth.ErrCertMismatch):
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
	case errors.Is(err, deviceauth.ErrUnknownUser), errors.Is(err, deviceauth.ErrUnknownDevice):
		httpapi.Fail(w, r, problem.NotFound(err.Error()))
	case errors.Is(err, deviceauth.ErrUserExists), errors.Is(err, deviceauth.ErrDeviceExists):
		httpapi.Fail(w, r, problem.Conflict(err.Error()))
	default:
		a.fail(w, r, "identity", err)
	}
}
