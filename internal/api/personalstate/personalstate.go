// Package personalstate is the DEVICE-facing side of the encrypted personal-state
// plane (§38, §40, §42, ADR-0049): the /api/v1 routes a device calls to push the
// opaque things it minted client-side — an EncryptedSpace, the wrapped copies of
// its key, and the encrypted CRDT changes — to the peer that stores them, and to
// fetch them back.
//
// Everything this API moves is opaque to the server. The space key is minted and
// wrapped on the device (client.Manager), never here; the peer stores wrapped
// keys it cannot unwrap and ciphertext changes it cannot read (Invariant 6). This
// package is a thin HTTP skin over internal/personalstate/store, which is exactly
// the boundary Invariant 4 wants: a device talks to its peer over HTTP, not an
// in-process pointer, so the same call works whether the device is this machine's
// CLI or a phone across the house (#330).
//
// It deliberately imports the store but NOT the client-side unwrap path or the
// CRDT model: the server has no way to turn a stored byte into plaintext, and
// this package adds none.
package personalstate

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// API serves the device-facing personal-state routes over an already-open peer
// store. A nil store is not constructible — New requires one — so unlike the peer
// surface's optional backends these routes are mounted only when the plane is
// wired, which it always is on a controller (the store is cheap and stateless).
type API struct {
	store      *store.Store
	replicator Replicator
	log        *slog.Logger
}

// Replicator runs one on-demand reconcile of this node's encrypted personal state
// to its trusted Full Peers (§37, §45), returning how many (peer, space) pairs
// converged and how many were deferred. It is the caller behind POST
// /api/v1/state/replicate; *personalstate/replication.Reconciler satisfies it.
//
// nil is legitimate — a node with no peer surface replicates to nobody, and the
// route answers 503 rather than 500.
type Replicator interface {
	ReconcileAll(ctx context.Context) (replicated, deferred int, err error)
}

// Options configure the API.
type Options struct {
	// Store is the peer-side opaque store (spaces, wrapped keys, changes).
	// Required — the API is nothing without it.
	Store *store.Store
	// Replicator drives on-demand replication to Full Peers. Optional: nil leaves
	// POST /state/replicate answering 503 (a node with no peers).
	Replicator Replicator
	Logger     *slog.Logger
}

// New builds the API. It fails if no store is given rather than mounting routes
// that 500 on the first call.
func New(opts Options) (*API, error) {
	if opts.Store == nil {
		return nil, errors.New("api/personalstate: a store is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &API{store: opts.Store, replicator: opts.Replicator, log: log.With("component", "personalstate-api")}, nil
}

// Mount registers the routes on the authenticated /api/v1 router. The scope on
// each route is the authorisation contract, stated here so a reviewer reads it
// off in one place (as internal/api/resources does): reading opaque state needs
// the `read` floor the router already requires; pushing it needs `write`.
//
// Note the authorisation/confidentiality split of ADR-0049: `write` lets a caller
// STORE ciphertext, and a `read` token lets it FETCH ciphertext — neither lets it
// READ the plaintext, which needs a wrapped key no token conveys.
func (a *API) Mount(r chi.Router) {
	r.Get("/spaces", a.listSpaces)
	r.Get("/spaces/{id}/keys", a.listWrappedKeys)
	r.Get("/spaces/{id}/changes", a.listChanges)
	r.Get("/spaces/{id}/snapshot", a.getSnapshot)

	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/spaces", a.createSpace)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/spaces/{id}/changes", a.putChange)
	// A snapshot is materialised and encrypted on the device (§44); pushing it is
	// a write, like a change.
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/spaces/{id}/snapshots", a.putSnapshot)
	// Compaction — dropping the changes a snapshot subsumes and every replica
	// holds — and triggering replication are operator actions on this node's
	// authority, so they need `admin`.
	r.With(httpapi.RequireScope(auth.ScopeAdmin)).Post("/spaces/{id}/compact", a.compact)
	r.With(httpapi.RequireScope(auth.ScopeAdmin)).Post("/state/replicate", a.replicate)
}

// replicateResult is the ack of an on-demand reconcile: how many (peer, space)
// pairs converged and how many were deferred (an unreachable peer, ADR-0038).
type replicateResult struct {
	Replicated int `json:"replicated"`
	Deferred   int `json:"deferred"`
}

func (a *API) replicate(w http.ResponseWriter, r *http.Request) {
	if a.replicator == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node replicates encrypted state to no peers (§37, §45)"))
		return
	}
	replicated, deferred, err := a.replicator.ReconcileAll(r.Context())
	if err != nil {
		a.log.Error("on-demand replication failed",
			"request_id", httpapi.RequestIDFrom(r.Context()), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	a.log.Info("replicated encrypted personal state", "replicated", replicated, "deferred", deferred)
	a.write(w, r, http.StatusOK, replicateResult{Replicated: replicated, Deferred: deferred})
}

// --- wire types ---------------------------------------------------------------

// wrappedKeyInput is one recipient's sealed copy of a space key as a device
// pushes it. Wrapped is opaque bytes (encryption.Seal output), base64 on the wire
// via encoding/json's []byte handling — the peer stores it and cannot open it.
type wrappedKeyInput struct {
	Recipient string `json:"recipient"`
	Wrapped   []byte `json:"wrapped"`
}

// createSpaceRequest is POST /spaces: a client-minted space and the wrapped
// copies of its key. The id and kind are the space (client-minted, §39); the
// keys are what makes it readable to the authorised devices and the recovery key.
type createSpaceRequest struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	WrappedKeys []wrappedKeyInput `json:"wrapped_keys"`
}

// spaceView is a space as the peer holds it — the opaque id, the structural kind,
// and when the peer recorded it. There is deliberately no name: a name is
// encrypted CRDT state, not metadata the peer stores (§38).
type spaceView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"created_at"`
}

func viewSpace(sp spaces.EncryptedSpace) spaceView {
	return spaceView{ID: sp.ID, Kind: string(sp.Kind), CreatedAt: sp.CreatedAt.UTC().Format(timeFormat)}
}

// spacesView is the list envelope.
type spacesView struct {
	Spaces []spaceView `json:"spaces"`
}

// wrappedKeyView is a stored wrapped key as a device fetches it to find the copy
// sealed for its own key. Wrapped is base64 opaque bytes.
type wrappedKeyView struct {
	Recipient string `json:"recipient"`
	Wrapped   []byte `json:"wrapped"`
	CreatedAt string `json:"created_at"`
}

// wrappedKeysView is the list envelope.
type wrappedKeysView struct {
	SpaceID     string           `json:"space_id"`
	WrappedKeys []wrappedKeyView `json:"wrapped_keys"`
}

// changesView is the list envelope for opaque changes a device pulls to merge.
// The element is protocol.EncryptedChange itself — its ciphertext is base64 and
// opaque; the peer never decrypted it.
type changesView struct {
	SpaceID string                     `json:"space_id"`
	Changes []protocol.EncryptedChange `json:"changes"`
}

// changeStored is the ack for a pushed change: the content-addressed id the peer
// verified and now holds.
type changeStored struct {
	ChangeID string `json:"change_id"`
}

const timeFormat = "2006-01-02T15:04:05.999999999Z07:00" // time.RFC3339Nano

// --- handlers -----------------------------------------------------------------

func (a *API) createSpace(w http.ResponseWriter, r *http.Request) {
	var req createSpaceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	sp, err := a.store.PutSpace(r.Context(), req.ID, spaces.Kind(req.Kind))
	if err != nil {
		a.failStore(w, r, "recording a space", err)
		return
	}
	// Wrap copies land after the space exists. Each is opaque; a bad recipient or
	// empty blob is a 400, not a partial success left to confuse the device.
	for _, k := range req.WrappedKeys {
		if _, err := a.store.PutWrappedKey(r.Context(), sp.ID, k.Recipient, k.Wrapped); err != nil {
			a.failStore(w, r, "recording a wrapped key", err)
			return
		}
	}
	a.write(w, r, http.StatusCreated, viewSpace(sp))
}

func (a *API) listSpaces(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListSpaces(r.Context())
	if err != nil {
		a.failStore(w, r, "listing spaces", err)
		return
	}
	out := spacesView{Spaces: make([]spaceView, 0, len(list))}
	for _, sp := range list {
		out.Spaces = append(out.Spaces, viewSpace(sp))
	}
	a.write(w, r, http.StatusOK, out)
}

func (a *API) listWrappedKeys(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "id")
	keys, err := a.store.WrappedKeysFor(r.Context(), spaceID)
	if err != nil {
		a.failStore(w, r, "listing wrapped keys", err)
		return
	}
	out := wrappedKeysView{SpaceID: spaceID, WrappedKeys: make([]wrappedKeyView, 0, len(keys))}
	for _, k := range keys {
		out.WrappedKeys = append(out.WrappedKeys, wrappedKeyView{
			Recipient: k.Recipient,
			Wrapped:   k.Wrapped,
			CreatedAt: k.CreatedAt.UTC().Format(timeFormat),
		})
	}
	a.write(w, r, http.StatusOK, out)
}

func (a *API) putChange(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "id")
	var ch protocol.EncryptedChange
	if err := decodeJSON(w, r, &ch); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	// The path names the space; the body must agree. A change re-pointed at
	// another space than the route it arrived on is a client bug or a relay
	// trick, and the store would refuse the id mismatch anyway — refuse it here
	// with a clearer message than "id does not match its content".
	if ch.SpaceID != spaceID {
		httpapi.Fail(w, r, problem.BadRequest("the change's space_id must match the route's space id"))
		return
	}
	if err := a.store.PutChange(r.Context(), ch); err != nil {
		a.failStore(w, r, "recording a change", err)
		return
	}
	a.write(w, r, http.StatusCreated, changeStored{ChangeID: ch.ChangeID})
}

func (a *API) listChanges(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "id")
	changes, err := a.store.ChangesFor(r.Context(), spaceID)
	if err != nil {
		a.failStore(w, r, "listing changes", err)
		return
	}
	a.write(w, r, http.StatusOK, changesView{SpaceID: spaceID, Changes: changes})
}

// --- helpers ------------------------------------------------------------------

// failStore maps a store/domain error to a problem document. An unknown space is
// a 404; a bad kind, a malformed id, an empty wrapped key or a change that fails
// its content-address check are the caller's 400; anything else is a 500 the
// caller cannot act on and the log carries.
func (a *API) failStore(w http.ResponseWriter, r *http.Request, doing string, err error) {
	switch {
	case errors.Is(err, store.ErrUnknownSpace):
		httpapi.Fail(w, r, problem.NotFound(err.Error()))
	case errors.Is(err, spaces.ErrUnknownKind),
		errors.Is(err, store.ErrEmptyRecipient),
		errors.Is(err, store.ErrEmptyWrapped),
		errors.Is(err, protocol.ErrIncomplete),
		errors.Is(err, protocol.ErrIDMismatch),
		errors.Is(err, protocol.ErrSnapshotIncomplete),
		errors.Is(err, protocol.ErrSnapshotIDMismatch):
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
	default:
		a.log.Error(doing+" failed",
			"request_id", httpapi.RequestIDFrom(r.Context()), "error", err)
		httpapi.Fail(w, r, problem.Internal())
	}
}

// write renders a successful JSON response with nosniff, mirroring the resources
// API's helper.
func (a *API) write(w http.ResponseWriter, r *http.Request, status int, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		a.log.Error("encoding a response failed",
			"request_id", httpapi.RequestIDFrom(r.Context()), "path", r.URL.Path, "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
