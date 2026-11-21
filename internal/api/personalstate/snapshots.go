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

// compactRequest names the snapshot to compact against, which becomes the
// space's pinned base (#681). Frontier is the shape this route took before: it
// is still decoded only so the caller is told plainly that it no longer counts.
type compactRequest struct {
	SnapshotID string   `json:"snapshot_id"`
	Frontier   []string `json:"frontier,omitempty"`
}

// compactResult is the ack: the base pinned, how many changes were compacted,
// and the trusted Full Peers whose acknowledgement does not yet name the base.
type compactResult struct {
	Base       string   `json:"base"`
	Pinned     bool     `json:"pinned"`
	Dropped    int      `json:"dropped"`
	WaitingFor []string `json:"waiting_for"`
}

// getSnapshot answers the snapshot a cold-starting device should start from: the
// newest that covers the pinned compaction base, else the base, else (no base)
// the newest (#681). The device verifies its envelope and falls back to
// getSnapshotBase when it does not verify.
func (a *API) getSnapshot(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "id")
	if !a.spaceAccess(w, r, spaceID, false) {
		return
	}
	snap, ok, err := a.store.ColdStartSnapshot(r.Context(), spaceID)
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

// getSnapshotBase answers the space's pinned compaction base alone, or 404 when
// nothing was ever compacted against a pin: then the whole log is still held, and
// a device that cannot verify a snapshot can pull it all.
func (a *API) getSnapshotBase(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "id")
	if !a.spaceAccess(w, r, spaceID, false) {
		return
	}
	snap, ok, err := a.store.Base(r.Context(), spaceID)
	if err != nil {
		a.failStore(w, r, "reading the compaction base", err)
		return
	}
	if !ok {
		httpapi.Fail(w, r, problem.NotFound("this space has no compaction base"))
		return
	}
	a.write(w, r, http.StatusOK, snap)
}

func (a *API) putSnapshot(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "id")
	if !a.spaceAccess(w, r, spaceID, true) {
		return
	}
	var snap protocol.EncryptedSnapshot
	// A snapshot folds a whole space, so it has its own, larger bound than the
	// rest of this surface; DecodeJSON reads it once, straight from the bounded
	// body, never into an intermediate buffer.
	if err := httpapi.DecodeJSON(w, r, &snap, protocol.MaxSnapshotBody); err != nil {
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
	if err := httpapi.DecodeJSON(w, r, &req, maxRequestBody); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if req.SnapshotID == "" {
		msg := "name the snapshot to compact against as snapshot_id"
		if req.Frontier != nil {
			msg = "compaction no longer takes a caller-supplied frontier: it is proven by what each trusted " +
				"Full Peer confirmed holding (#681); " + msg
		}
		httpapi.Fail(w, r, problem.BadRequest(msg))
		return
	}
	var peers []string
	if a.replicator != nil {
		var err error
		if peers, err = a.replicator.FullPeerIDs(r.Context()); err != nil {
			a.failStore(w, r, "listing the trusted Full Peers", err)
			return
		}
	}
	res, err := a.store.CompactChanges(r.Context(), spaceID, req.SnapshotID, peers)
	if err != nil {
		a.failStore(w, r, "compacting changes", err)
		return
	}
	waiting := res.Waiting
	if waiting == nil {
		waiting = []string{}
	}
	a.write(w, r, http.StatusOK, compactResult{Base: res.Base, Pinned: res.Pinned, Dropped: res.Dropped, WaitingFor: waiting})
}
