// Package relay mounts the Voidbind pairing relay — voidbind-go's relay.Server —
// on the node's PUBLIC router, under httpapi.RelayV1Prefix (ADR-0066).
//
// It is the node's only pairing relay. The Voidbind clients — `heyarr pair`,
// the voidbind CLI's pair-initiate / pair-join and the phone's voidbind-kmp —
// speak voidbind-go's relay protocol: POST /v1/sessions, then
// PUT|GET /v1/sessions/{id}/{role}/{type} with a JSON reveal and an
// X25519-sealed admission. The legacy slot relay that heyarr ran at
// /pair/sessions/{s}/slots/{slot} before the Voidbind extraction is retired
// (ADR-0066, #627).
//
// Like every other public mount, this grants nothing (ADR-0038): the relay is an
// opaque, write-once, in-memory blob store that never parses a payload and holds
// no key; the security is the commit-before-reveal and the SAS on the two
// clients, and the admission crosses it sealed to the new device's encryption
// key. Serving it without a credential adds no authority to anyone. A public
// route still carries caps: a per-message byte cap, a live session cap and a
// TTL.
package relay

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	vbrelay "github.com/rarebit-one/voidbind-go/relay"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
)

// Options configure a Handler. The zero value is the production configuration.
type Options struct {
	// Now is the clock the relay's session TTL is evicted against (ADR-0017);
	// nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
	// Types is the message-slot allow-list the relay accepts. Empty keeps
	// voidbind-go's pairing default (commit/reveal/cert). A node that also carries
	// the cruciform-offload live path sets it to the pairing set plus the offload
	// slots — the caller composes them, so this package stays agnostic to what
	// rides the relay (see the controller mount).
	Types []string
}

// Handler serves the Voidbind relay under httpapi.RelayV1Prefix.
type Handler struct {
	routes http.Handler
	log    *slog.Logger
}

// MaxMessageBytes bounds one relay slot. The retired legacy relay's 4 KiB was
// sized for a bare commit/reveal; since ADR-0068 the sealed `cert` slot carries the
// admitting op AND the initiator's known membership ops (up to
// rp.MaxPresentedOps of ~700 B each), and a phone with a few devices overflowed
// it with a 413 mid-pairing. voidbind-go's own relay default (64 KiB) is the
// wire's stated bound, so the node uses the same number.
const MaxMessageBytes = vbrelay.DefaultMaxMessageBytes

// MaxSessions is how many live pairing sessions the relay holds at once. A
// homelab pairs a handful of devices, rarely at the same time; the cap only has
// to stop an unauthenticated caller exhausting memory with sessions.
const MaxSessions = 256

// SessionTTL is how long an idle session lives before it is evictable. A
// pairing completes in seconds, or is abandoned.
const SessionTTL = 10 * time.Minute

// New builds the relay with the node's session caps and the voidbind-go relay's
// slot cap.
func New(opts Options) *Handler {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	srv := vbrelay.NewServer(vbrelay.Options{
		MaxMessageBytes: MaxMessageBytes,
		MaxSessions:     MaxSessions,
		SessionTTL:      SessionTTL,
		Now:             opts.Now,
		Types:           opts.Types,
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
