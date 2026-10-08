package personalstate

// keyepoch.go is the device-facing side of a space key's epoch and history
// (ADR-0103, #698): a device fetches the opaque chain that lets it read content
// sealed under earlier keys, and rotates a space by handing the peer the
// previous key sealed under the new one plus the new key's wrapped copies. The
// peer opens none of it (Invariant 6); it enforces only that a rotation starts
// from the current epoch.

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// The problem codes a key-epoch refusal carries (all 409s), so a device branches
// on the code rather than the prose detail.
const (
	// CodeKeyEpochStale: the wrap seals an epoch older than the space's current
	// one — the key was rotated since.
	CodeKeyEpochStale = "key_epoch_stale"
	// CodeKeyEpochAhead: the wrap seals an epoch the peer has not reached.
	CodeKeyEpochAhead = "key_epoch_ahead"
	// CodeKeyEpochConflict: a rotation's expected_epoch is not the current one —
	// another rotation landed first.
	CodeKeyEpochConflict = "key_epoch_conflict"
	// CodeRotationDropsRecovery: a rotation left out a recovery key that holds a
	// copy of the current key (ADR-0022, ADR-0103).
	CodeRotationDropsRecovery = "rotation_drops_recovery_key"
	// CodeRotationRecipientsChanged: a rotation's wraps and revoke set do not
	// account for exactly the recipients holding a copy of the current key — one
	// was added or removed since the device read them (#703).
	CodeRotationRecipientsChanged = "rotation_recipients_changed"
)

// keyHistoryEntryView is one link of the chain: the key of epoch Epoch-1 sealed
// under the key of epoch Epoch. SealedPrev is base64 opaque bytes.
type keyHistoryEntryView struct {
	Epoch      int    `json:"epoch"`
	SealedPrev []byte `json:"sealed_prev"`
	CreatedAt  string `json:"created_at"`
}

// keyHistoryView is GET /spaces/{id}/key-history, oldest epoch first.
type keyHistoryView struct {
	SpaceID string                `json:"space_id"`
	Entries []keyHistoryEntryView `json:"entries"`
}

// rotateRequest is POST /spaces/{id}/rotate: the epoch the device rotated FROM
// and the recipient set it read (together the compare-and-swap), the previous
// key sealed under the new one, and the new key's wrapped copies. Revoke names
// the current recipients the rotation leaves out, who lose their copy; it is
// required (an empty list revokes nobody), so a recipient is never dropped by
// omission (#703).
type rotateRequest struct {
	ExpectedEpoch int               `json:"expected_epoch"`
	SealedPrev    []byte            `json:"sealed_prev"`
	WrappedKeys   []wrappedKeyInput `json:"wrapped_keys"`
	Revoke        *[]string         `json:"revoke"`
}

// rotateResult acks a rotation with the space's new epoch.
type rotateResult struct {
	SpaceID  string `json:"space_id"`
	KeyEpoch int    `json:"key_epoch"`
}

func (a *API) listKeyHistory(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "id")
	if !a.spaceAccess(w, r, spaceID, false) {
		return
	}
	entries, err := a.store.KeyHistory(r.Context(), spaceID)
	if err != nil {
		a.failStore(w, r, "listing key history", err)
		return
	}
	out := keyHistoryView{SpaceID: spaceID, Entries: make([]keyHistoryEntryView, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, keyHistoryEntryView{
			Epoch:      e.Epoch,
			SealedPrev: e.SealedPrev,
			CreatedAt:  e.CreatedAt.UTC().Format(timeFormat),
		})
	}
	a.write(w, r, http.StatusOK, out)
}

// rotateKey moves a space to its next key epoch (ADR-0103). Every wrap is held to
// enrol-before-wrap like create and re-wrap; a per-wrap epoch is meaningless here
// (the rotation decides it) and is refused rather than ignored. A rotation that
// lost a race — to another rotation, or to a recipient added or removed since
// the device read the set — is a 409; the device re-opens and rotates again.
func (a *API) rotateKey(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "id")
	var req rotateRequest
	if err := httpapi.DecodeJSON(w, r, &req, maxRequestBody); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if len(req.WrappedKeys) == 0 {
		httpapi.Fail(w, r, problem.BadRequest("a rotation needs at least one wrapped key"))
		return
	}
	if req.Revoke == nil {
		httpapi.Fail(w, r, problem.BadRequest(
			"a rotation must name the recipients it revokes in revoke (an empty list revokes nobody)"))
		return
	}
	wraps := make([]store.RecipientWrap, 0, len(req.WrappedKeys))
	for _, k := range req.WrappedKeys {
		if k.Epoch != 0 {
			httpapi.Fail(w, r, problem.BadRequest("a rotation's wrapped keys carry no epoch — the rotation assigns expected_epoch+1"))
			return
		}
		wraps = append(wraps, store.RecipientWrap{Recipient: k.Recipient, Wrapped: k.Wrapped})
	}
	if !a.recipientsAllowed(w, r, req.WrappedKeys) {
		return
	}
	// The recovery keys a rotation must keep. With no authorizer wired the check
	// is off, like enrol-before-wrap (the pre-M9 behaviour); a controller wires
	// one, and the device store is what knows which pinned keys are recovery
	// keys — the peer store only sees opaque recipient ids.
	var preserve map[string]bool
	if a.authorizer != nil {
		var err error
		if preserve, err = a.authorizer.RecoveryWrapRecipients(r.Context()); err != nil {
			a.log.Error("listing recovery recipients", "error", err)
			httpapi.Fail(w, r, problem.Internal())
			return
		}
	}
	epoch, err := a.store.RotateKey(r.Context(), spaceID, req.ExpectedEpoch, req.SealedPrev, wraps, *req.Revoke, preserve)
	if err != nil {
		a.failStore(w, r, "rotating a space key", err)
		return
	}
	a.log.Info("rotated a space key", "space", spaceID, "epoch", epoch, "recipients", len(wraps), "revoked", len(*req.Revoke))
	a.write(w, r, http.StatusOK, rotateResult{SpaceID: spaceID, KeyEpoch: epoch})
}
