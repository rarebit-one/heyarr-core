package personalstate

// snapshots.go is the device-facing side of §44's snapshots and compaction: a
// device materialises and encrypts a snapshot and pushes it here; a joining
// device fetches the latest; and an operator triggers compaction. Everything is
// opaque — the snapshot is ciphertext, the compaction bound is causal metadata —
// and the server materialises nothing.

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

// compactRequest is the acknowledged causal frontier — the frontier every trusted
// Full Peer has confirmed holding, so a change within it is safe to drop (§44).
type compactRequest struct {
	Frontier []string `json:"frontier"`
}

// compactResult is the ack: how many changes were compacted.
type compactResult struct {
	Dropped int `json:"dropped"`
}

func (a *API) getSnapshot(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "id")
	snap, ok, err := a.store.LatestSnapshotFor(r.Context(), spaceID)
	if err != nil {
		a.failStore(w, r, "reading a snapshot", err)
		return
	}
	if !ok {
		httpapi.Fail(w, r, problem.NotFound("this space has no snapshot"))
		return
	}
	a.write(w, r, http.StatusOK, snap)
}

func (a *API) putSnapshot(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "id")
	var snap protocol.EncryptedSnapshot
	if err := decodeJSON(w, r, &snap); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if snap.SpaceID != spaceID {
		httpapi.Fail(w, r, problem.BadRequest("the snapshot's space_id must match the route's space id"))
		return
	}
	if err := a.store.PutSnapshot(r.Context(), snap); err != nil {
		a.failStore(w, r, "recording a snapshot", err)
		return
	}
	a.write(w, r, http.StatusCreated, snapshotStored{SnapshotID: snap.SnapshotID})
}

// snapshotStored is the push ack.
type snapshotStored struct {
	SnapshotID string `json:"snapshot_id"`
}

func (a *API) compact(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "id")
	var req compactRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	dropped, err := a.store.CompactChanges(r.Context(), spaceID, req.Frontier)
	if err != nil {
		a.failStore(w, r, "compacting changes", err)
		return
	}
	a.write(w, r, http.StatusOK, compactResult{Dropped: dropped})
}
