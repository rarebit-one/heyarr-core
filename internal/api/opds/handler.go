package opds

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Prefix is where the OPDS adapter is mounted. Like the renderer, pairing and
// OpenSubsonic surfaces it lives OUTSIDE the authenticated /api/v1 group: an
// OPDS reader authenticates with HTTP Basic in the protocol's own terms, not a
// Heyarr bearer credential in the group's scheme, so the group's guard would
// challenge it in a way no reader answers.
const Prefix = "/opds"

// Authenticator verifies a Heyarr bearer credential — the subset of
// *auth.Verifier this adapter needs, an interface so a test injects a fake
// without paying argon2id.
type Authenticator interface {
	Verify(ctx context.Context, raw string) (auth.Identity, error)
}

// Options builds a Handler.
type Options struct {
	// DB is the controller database, read through its reader pool. The adapter
	// is a read-only PROJECTION over the publication catalogue (§69, §70): it
	// never writes and never reaches the encrypted personal-state plane, which
	// the server cannot decrypt anyway (Invariant 6, §72). Reading position and
	// shelves are therefore out of scope here by construction and belong to a
	// device-side Personal MCP (§73), not this controller-side adapter.
	DB *sqlite.DB
	// Auth verifies the bearer token an OPDS reader sends as its Basic password.
	Auth Authenticator
	// Blobs is the ordinary blob handler. The download route resolves a
	// publication to a blob hash and hands byte-serving to it verbatim
	// (ADR-0013): Range and 206 are inherited, not reimplemented.
	Blobs  *blobs.Handler
	Logger *slog.Logger
	// Now is injected so a feed's timestamp is testable; defaults to time.Now.
	Now func() time.Time
}

// Handler is the OPDS compatibility adapter for publications (§69, §70).
type Handler struct {
	reader *sql.DB
	auth   Authenticator
	blobs  *blobs.Handler
	log    *slog.Logger
	now    func() time.Time
}

// New builds the handler, refusing a mis-wired one at construction.
func New(opts Options) (*Handler, error) {
	if opts.DB == nil {
		return nil, errors.New("opds: a database is required")
	}
	if opts.Auth == nil {
		return nil, errors.New("opds: an authenticator is required")
	}
	if opts.Blobs == nil {
		return nil, errors.New("opds: a blob handler is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Handler{
		reader: opts.DB.Reader(),
		auth:   opts.Auth,
		blobs:  opts.Blobs,
		log:    log.With("component", "opds"),
		now:    now,
	}, nil
}

// Mount registers the adapter on an UNAUTHENTICATED router (see Prefix). Three
// routes: the root menu, the one acquisition feed, and the byte download. Few
// and fixed on purpose — the route-parity guard (ADR-0015) documents each.
func (h *Handler) Mount(r chi.Router) {
	r.Get(Prefix, h.authed(h.handleRoot))
	r.Get(Prefix+"/publications", h.authed(h.handlePublications))
	r.Get(Prefix+"/download/{id}", h.authed(h.handleDownload))
	r.Head(Prefix+"/download/{id}", h.authed(h.handleDownload))
}

// authed wraps a handler with HTTP Basic authentication.
//
// An OPDS reader sends `Authorization: Basic base64(user:password)`, and the
// password is a Heyarr bearer token verified against the same store the whole
// API uses — so a token's scope and revocation apply here identically. Unlike
// the OpenSubsonic adapter, which carries status inside its own envelope, OPDS
// speaks plain HTTP: a missing or bad credential is a real 401 with a Basic
// challenge, which is exactly what a reader waits for before it prompts.
func (h *Handler) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch h.authenticate(r) {
		case http.StatusUnauthorized:
			h.challenge(w)
		case http.StatusForbidden:
			http.Error(w, "the credential is not allowed to read the library", http.StatusForbidden)
		default:
			next(w, r)
		}
	}
}

// authenticate checks that the request's Basic password is a Heyarr token with
// at least `read`. It returns http.StatusOK when it is, and otherwise the
// status to refuse with.
//
// It deliberately hands no auth.Identity on to the handlers, for the same
// reasons as the OpenSubsonic adapter's authenticate (#619). Every
// identity-scoped behaviour on the REST surface keys on something an OPDS
// caller cannot have. Guest restrictions (ADR-0074, ADR-0094), including the
// guest.Visible asset filter, key on Identity.Guest. Only the credential-less
// guest admission inside the /api/v1 group sets that flag. This adapter mounts
// outside that group and always demands a bearer token, which Verify never
// resolves to a guest. Per-identity state (reading position, bookmarks,
// shelves) is encrypted personal state the controller cannot read (Invariant 6,
// ADR-0054), and this adapter does not serve any of it. Downloads go to
// internal/api/blobs, which does not look at identity either. What is left is
// one shared publication catalogue gated by scope, so `read` is the whole
// authorisation decision. If a finer, per-identity grant ever reaches the
// catalogue, the identity has to start flowing into the handlers here.
func (h *Handler) authenticate(r *http.Request) int {
	_, password, ok := r.BasicAuth()
	if !ok || password == "" {
		return http.StatusUnauthorized
	}
	id, err := h.auth.Verify(r.Context(), password)
	if err != nil {
		return http.StatusUnauthorized
	}
	if !id.Allows(auth.ScopeRead) {
		return http.StatusForbidden
	}
	return http.StatusOK
}

func (h *Handler) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="Heyarr"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

// handleRoot is the navigation feed: a menu whose single entry descends into
// the acquisition feed. A reader opens this first.
func (h *Handler) handleRoot(w http.ResponseWriter, r *http.Request) {
	stamp := h.now().UTC().Format(time.RFC3339)
	writeFeed(w, navType, feed{
		ID:      "urn:heyarr:opds:root",
		Title:   "Heyarr",
		Updated: stamp,
		Links: []link{
			{Rel: relSelf, Href: Prefix, Type: navType},
			{Rel: relStart, Href: Prefix, Type: navType},
		},
		Entries: []entry{{
			Title:   "All Publications",
			ID:      "urn:heyarr:opds:publications",
			Updated: stamp,
			Links:   []link{{Rel: relSubsection, Href: Prefix + "/publications", Type: acquisitionType}},
			Content: &content{Type: "text", Text: "Every book, comic and document in the library."},
		}},
	})
}
