package pairrelay

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
)

// Handler serves the relay over HTTP. It is mounted on the UNAUTHENTICATED
// router (like the renderer route, ADR-0040), because a device being paired has
// no credential and the relay grants no authority — see the package doc and
// httpapi.RelayPrefix.
type Handler struct {
	store *Store
	log   *slog.Logger
}

// HandlerOptions configure a Handler.
type HandlerOptions struct {
	Store  *Store
	Logger *slog.Logger
}

// NewHandler constructs a Handler. A nil store gets a default one, so a caller
// that only wants the routes mounted need not build state it will not tune.
func NewHandler(opts HandlerOptions) *Handler {
	store := opts.Store
	if store == nil {
		store = New(Options{})
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Handler{store: store, log: log.With("component", "pairrelay")}
}

// Mount registers the relay routes on an unauthenticated router.
//
// Two operations on one path: PUT stores a slot value, GET reads it. The path
// carries the session and the slot; neither is a secret (a session id is a
// rendezvous label and a slot name is fixed), so unlike the render route there
// is nothing to redact here.
func (h *Handler) Mount(r chi.Router) {
	r.Put(httpapi.RelayPrefix+"/sessions/{session}/slots/{slot}", h.put)
	r.Get(httpapi.RelayPrefix+"/sessions/{session}/slots/{slot}", h.get)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	session := chi.URLParam(r, "session")
	slot := chi.URLParam(r, "slot")
	// Bound the read at one slot past the cap, so an oversized body is refused
	// rather than buffered whole.
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxSlotBytes+1))
	if err != nil {
		http.Error(w, "reading the request body", http.StatusBadRequest)
		return
	}
	if len(body) > MaxSlotBytes {
		http.Error(w, ErrTooLarge.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	switch err := h.store.Put(session, slot, body); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrUnknownSlot), errors.Is(err, ErrBadSession):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	case errors.Is(err, ErrSlotConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrTooManySessions):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	default:
		h.log.Error("storing a relay slot failed", "error", err)
		http.Error(w, "relay error", http.StatusInternalServerError)
	}
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	session := chi.URLParam(r, "session")
	slot := chi.URLParam(r, "slot")
	data, err := h.store.Get(session, slot)
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		// The relay hands back the opaque bytes a peer stored (a commitment, a
		// public key, a salt or a cert), never HTML. The octet-stream type and
		// the nosniff header above are the mitigation — a browser will not
		// render this as a document — so echoing the stored value is safe.
		_, _ = w.Write(data) //nolint:gosec // G705: opaque relay bytes served as application/octet-stream with nosniff, never as HTML
	case errors.Is(err, ErrBadSession):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		h.log.Error("reading a relay slot failed", "error", err)
		http.Error(w, "relay error", http.StatusInternalServerError)
	}
}
