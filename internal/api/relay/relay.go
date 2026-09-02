// Package relay mounts the Voidbind pairing relay — voidbind-go's relay.Server —
// on the node's PUBLIC router, under httpapi.RelayV1Prefix (ADR-0066).
//
// It exists because a phone does not speak heyarr's own relay. internal/pairflow
// and internal/pairrelay predate the Voidbind extraction and speak a private
// slot protocol (/pair/sessions/{s}/slots/{slot}, uvarint-framed reveals, a
// plaintext cert). The Voidbind clients — the voidbind CLI's pair-initiate /
// pair-join and the phone's voidbind-kmp — speak voidbind-go's relay protocol
// instead: POST /v1/sessions, then PUT|GET /v1/sessions/{id}/{role}/{type} with
// a JSON reveal and an X25519-sealed cert. A node that hosts only the legacy
// relay cannot be the rendezvous for its own phone; this package makes it one.
//
// The mount is ADDITIVE. The legacy relay stays at /pair for `heyarr pair`, and
// this one sits beside it at /pair/v1; neither can be confused for the other
// because the legacy relay has no "/v1/sessions" path and this one has no
// "/sessions/{s}/slots" path. Retiring the legacy pair is a separate change
// (ADR-0066 records why it is not this one).
//
// Like the legacy relay (ADR-0038) and every other public mount, this grants
// nothing: the relay is an opaque, write-once, in-memory blob store that never
// parses a payload and holds no key; the security is the commit-before-reveal
// and the SAS on the two clients, and the cert crosses it sealed to the new
// device's encryption key. Serving it without a credential adds no authority to
// anyone. The caps a public route carries — a per-message byte cap, a live
// session cap and a TTL — are the SAME values the legacy relay enforces, so the
// two mounts present one abuse surface rather than two.
package relay

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	vbrelay "github.com/rarebit-one/voidbind-go/relay"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/pairrelay"
)

// Options configure a Handler. The zero value is the production configuration.
type Options struct {
	// Now is the clock the relay's session TTL is evicted against (ADR-0017);
	// nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Handler serves the Voidbind relay under httpapi.RelayV1Prefix.
type Handler struct {
	routes http.Handler
	log    *slog.Logger
}

// New builds the relay with the legacy relay's caps.
func New(opts Options) *Handler {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	srv := vbrelay.NewServer(vbrelay.Options{
		MaxMessageBytes: pairrelay.MaxSlotBytes,
		MaxSessions:     pairrelay.MaxSessions,
		SessionTTL:      pairrelay.SessionTTL,
		Now:             opts.Now,
	})
	// The relay's own mux is written against "/v1/..." (the origin-relative
	// paths a `voidbind relay` serves) and voidbind-go's client appends those
	// paths to its configured base, so the node's prefix is stripped before the
	// request reaches the mux: a client whose relay base is "<node>/pair" dials
	// "<node>/pair/v1/sessions", exactly the paths a standalone relay serves,
	// and they land under httpapi.RelayV1Prefix.
	return &Handler{
		routes: http.StripPrefix(httpapi.RelayPrefix, srv.Routes()),
		log:    log.With("component", "relay-v1"),
	}
}

// Mount registers the relay on an unauthenticated router (an httpapi.MountFunc).
//
// The three operations are registered one by one rather than as a wildcard so
// the router walk the OpenAPI parity test performs (ADR-0015) sees exactly the
// routes the specification documents. Beneath them the relay's own Go 1.22
// method+wildcard mux re-matches the same path, so a role or type it does not
// know is refused by the relay's rules, not by a second copy of them here.
func (h *Handler) Mount(r chi.Router) {
	r.Post(httpapi.RelayV1Prefix+"/sessions", h.routes.ServeHTTP)
	r.Put(httpapi.RelayV1Prefix+"/sessions/{id}/{role}/{type}", h.routes.ServeHTTP)
	r.Get(httpapi.RelayV1Prefix+"/sessions/{id}/{role}/{type}", h.routes.ServeHTTP)
}
