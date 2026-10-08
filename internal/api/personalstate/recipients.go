package personalstate

// recipients.go registers service recipients (ADR-0104): an executor's X25519
// public key, pinned as a wrap target for one executor principal by an owner's
// management-authorised device. An executor is never enrolled as a member
// device, so without this registration enrol-before-wrap (ADR-0049) refuses
// every wrap for it. Registering a key does not wrap anything: a key is a wrap
// target only on a space where its executor holds a grant, and the grant is
// what carries the wrap (grants.go).

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/servicerecipient"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// CodeFingerprintMismatch is a registration whose typed fingerprint is not the
// key's: the key the device was handed is not the one the executor's host
// displayed.
const CodeFingerprintMismatch = "recipient_fingerprint_mismatch"

// registerRecipientRequest is POST /service-recipients. Fingerprint is the one
// the operator read off the executor's host; when present it must match the
// key's, so a key substituted in transit is refused here as well as in the CLI.
type registerRecipientRequest struct {
	Principal   string `json:"principal"`
	Recipient   string `json:"recipient"`
	Label       string `json:"label,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// serviceRecipientView is a registered service recipient.
type serviceRecipientView struct {
	ID                 string `json:"id"`
	PrincipalID        string `json:"principal_id"`
	Recipient          string `json:"recipient"`
	Label              string `json:"label,omitempty"`
	Fingerprint        string `json:"fingerprint"`
	RegisteredByDevice string `json:"registered_by_device"`
	CreatedAt          string `json:"created_at"`
}

func viewServiceRecipient(sr store.ServiceRecipient) serviceRecipientView {
	return serviceRecipientView{
		ID: sr.ID, PrincipalID: sr.PrincipalID, Recipient: sr.Recipient, Label: sr.Label,
		Fingerprint: sr.Fingerprint, RegisteredByDevice: sr.RegisteredByDevice,
		CreatedAt: sr.CreatedAt.UTC().Format(timeFormat),
	}
}

type serviceRecipientsView struct {
	Recipients []serviceRecipientView `json:"recipients"`
}

// removedRecipientView acks a removal with the spaces whose wrapped copy for
// the key was deleted with it.
type removedRecipientView struct {
	serviceRecipientView
	WrapsDeletedFrom []string `json:"wraps_deleted_from"`
}

// maxLabel bounds an operator's note.
const maxLabel = 200

func (a *API) registerRecipient(w http.ResponseWriter, r *http.Request) {
	id, ok := granter(w, r)
	if !ok {
		return
	}
	if a.principals == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeServiceUnavailable,
			"Service Unavailable", "this node resolves no executor principals"))
		return
	}
	var req registerRecipientRequest
	if err := httpapi.DecodeJSON(w, r, &req, maxRequestBody); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if len(req.Label) > maxLabel {
		httpapi.Fail(w, r, problem.BadRequest("a recipient's label is at most 200 bytes"))
		return
	}
	fp, err := servicerecipient.Fingerprint(req.Recipient)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if req.Fingerprint != "" && !servicerecipient.SameFingerprint(req.Fingerprint, fp) {
		httpapi.Fail(w, r, problem.BadRequest(
			"the fingerprint given is not this key's: the key is not the one the executor's host displayed").
			WithCode(CodeFingerprintMismatch))
		return
	}
	principal, ok := a.resolveExecutor(w, r, req.Principal)
	if !ok {
		return
	}
	// A member device's or a recovery key is never also a service recipient:
	// the two classes are revoked differently, and conflating them would let
	// removing one silently strip the other.
	if a.authorizer != nil {
		pinned, err := a.authorizer.AllowedWrapRecipients(r.Context())
		if err != nil {
			a.log.Error("checking wrap recipients", "error", err)
			httpapi.Fail(w, r, problem.Internal())
			return
		}
		if pinned[req.Recipient] {
			httpapi.Fail(w, r, problem.Conflict(
				"that key is an enrolled device's or a recovery key, not an executor's; an executor never shares a key with a member device"))
			return
		}
	}
	sr, created, err := a.store.RegisterServiceRecipient(r.Context(), principal.ID, req.Recipient, req.Label, fp,
		id.Principal.ID, id.DeviceKey)
	if err != nil {
		a.failRecipient(w, r, "registering a service recipient", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		a.log.Info("registered a service recipient", "id", sr.ID, "principal", sr.PrincipalID)
	}
	a.write(w, r, status, viewServiceRecipient(sr))
}

func (a *API) listRecipients(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ServiceRecipients(r.Context())
	if err != nil {
		a.failStore(w, r, "listing service recipients", err)
		return
	}
	out := serviceRecipientsView{Recipients: make([]serviceRecipientView, 0, len(list))}
	for _, sr := range list {
		out.Recipients = append(out.Recipients, viewServiceRecipient(sr))
	}
	a.write(w, r, http.StatusOK, out)
}

// removeRecipient withdraws a registration, by id or by key, and deletes every
// wrapped copy held for that key on this node in the same transaction.
func (a *API) removeRecipient(w http.ResponseWriter, r *http.Request) {
	id, ok := granter(w, r)
	if !ok {
		return
	}
	sr, spaceIDs, err := a.store.RemoveServiceRecipient(r.Context(), chi.URLParam(r, "ref"), id.DeviceKey)
	if err != nil {
		a.failRecipient(w, r, "removing a service recipient", err)
		return
	}
	a.log.Info("removed a service recipient", "id", sr.ID, "principal", sr.PrincipalID, "wraps_deleted", len(spaceIDs))
	if spaceIDs == nil {
		spaceIDs = []string{}
	}
	a.write(w, r, http.StatusOK, removedRecipientView{serviceRecipientView: viewServiceRecipient(sr), WrapsDeletedFrom: spaceIDs})
}

func (a *API) failRecipient(w http.ResponseWriter, r *http.Request, doing string, err error) {
	switch {
	case errors.Is(err, store.ErrRecipientTaken), errors.Is(err, store.ErrRecipientIsMemberKey):
		httpapi.Fail(w, r, problem.Conflict(err.Error()))
	case errors.Is(err, store.ErrUnknownRecipient):
		httpapi.Fail(w, r, problem.NotFound(err.Error()))
	default:
		a.failStore(w, r, doing, err)
	}
}
