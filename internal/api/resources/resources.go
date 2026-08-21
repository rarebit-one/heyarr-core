// Package resources serves the JSON API's collections and items (spec §77).
//
// It is mounted onto the HTTP foundation through httpapi.MountFunc rather than
// being wired into the server, so every route here inherits the whole
// middleware chain and the `read` scope floor without having to remember to.
// A route that needs more says so at the route.
//
// Two rules run through the whole package:
//
//   - Collections are paginated by keyset with an opaque cursor, never by
//     OFFSET. A library is written to while it is being browsed, and OFFSET
//     both skips and repeats rows under concurrent inserts.
//   - An unknown identifier is a 404 problem document, never an empty 200. A
//     client cannot tell "no such work" from "a work with nothing in it" if
//     both are 200, and it is the difference between retrying and giving up.
package resources

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// registryOrEmpty makes a nil registry behave like an empty one.
//
// A node with no providers is a supported configuration (ADR-0025), so every
// read path would otherwise need a nil check — and the one that forgot would
// panic on the status endpoint, which is the endpoint an operator reaches for
// precisely when things are already degraded.
func registryOrEmpty(r *providers.Registry) *providers.Registry {
	if r != nil {
		return r
	}
	return providers.New(nil)
}

// Options configure the resource API.
type Options struct {
	// DB is the controller database. Reads go to the reader pool so they never
	// queue behind a write; writes go through DB.InTx on the single writer.
	DB *sqlite.DB
	// Jobs enqueues scans and retries.
	Jobs *jobs.Queue
	// Events appends the event for every mutation and feeds the SSE stream.
	Events *events.Log
	// Tokens backs the credential endpoints.
	Tokens *auth.Store
	// Catalog answers §56's two satisfaction questions.
	//
	// Optional: the resource API is useful without it, and a nil catalog
	// simply means the satisfaction endpoints are not mounted. That keeps the
	// test harnesses that predate M3-05 working unchanged rather than making
	// every one of them construct a catalog to exercise an unrelated route.
	Catalog *catalog.Catalog
	// Providers is the centralised provider registry (§59). Optional: nil
	// means this node has none configured, which is a supported state
	// (ADR-0025) rather than a wiring mistake — so GET /api/v1/providers
	// reports an empty set rather than not being mounted. An operator asking
	// "what indexers do I have" deserves the answer "none" rather than a 404.
	Providers *providers.Registry
	Logger    *slog.Logger
	// Now and NewID are injected so that a created resource's timestamp and
	// identifier are fixed values in a test, which is what lets the response
	// shapes be golden files rather than a regex (ADR-0017).
	Now   func() time.Time
	NewID func() string
	// StreamHeartbeat is how often the SSE stream writes a keep-alive comment.
	// Zero means the default.
	StreamHeartbeat time.Duration
	// StreamPoll is how often an open SSE connection re-reads the event log for
	// events emitted by another process. Zero means defaultStreamPoll.
	StreamPoll time.Duration
	// StreamBuffer is how many events may queue for one SSE connection before
	// the log gives up on it. Zero means the default. It is configurable so a
	// test can actually drive the drop path rather than assuming it works.
	StreamBuffer int
}

// API holds the handlers.
type API struct {
	db        *sqlite.DB
	reader    *sql.DB
	jobs      *jobs.Queue
	events    *events.Log
	tokens    *auth.Store
	catalog   *catalog.Catalog
	providers *providers.Registry
	log       *slog.Logger
	now       func() time.Time
	newID     func() string

	heartbeat  time.Duration
	streamPoll time.Duration
	buffer     int
}

// defaultHeartbeat keeps an idle SSE connection alive through proxies that time
// out a silent stream. It is a comment line, so a client never sees it as an
// event.
const defaultHeartbeat = 15 * time.Second

// defaultStreamPoll is how often an open stream re-reads the log.
//
// It is the whole latency budget for an event emitted by a role other than the
// one serving this connection — see the "why this polls" note in stream.go.
// One second is short enough that `events tail` feels live and long enough that
// an idle connection costs one indexed lookup per second on the events primary
// key.
const defaultStreamPoll = time.Second

// New constructs the resource API.
func New(opts Options) (*API, error) {
	if opts.DB == nil {
		return nil, errors.New("resources: a database is required")
	}
	if opts.Jobs == nil {
		return nil, errors.New("resources: a job queue is required")
	}
	if opts.Events == nil {
		return nil, errors.New("resources: an event log is required")
	}
	if opts.Tokens == nil {
		return nil, errors.New("resources: a token store is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	newID := opts.NewID
	if newID == nil {
		newID = func() string { return uuid.Must(uuid.NewV7()).String() }
	}
	heartbeat := opts.StreamHeartbeat
	if heartbeat <= 0 {
		heartbeat = defaultHeartbeat
	}
	streamPoll := opts.StreamPoll
	if streamPoll <= 0 {
		streamPoll = defaultStreamPoll
	}
	buffer := opts.StreamBuffer
	if buffer <= 0 {
		buffer = subscriberBuffer
	}
	return &API{
		db:         opts.DB,
		reader:     opts.DB.Reader(),
		jobs:       opts.Jobs,
		events:     opts.Events,
		tokens:     opts.Tokens,
		catalog:    opts.Catalog,
		providers:  registryOrEmpty(opts.Providers),
		log:        log.With("component", "api"),
		now:        now,
		newID:      newID,
		heartbeat:  heartbeat,
		streamPoll: streamPoll,
		buffer:     buffer,
	}, nil
}

// Mount registers every resource route on the authenticated /api/v1 router.
//
// The scope on each route is the authorisation contract, and it is stated here
// rather than inside the handlers so that it can be read off in one screen: a
// reviewer should be able to see that nothing mutating is reachable with a read
// token without opening ten files.
func (a *API) Mount(r chi.Router) {
	// Reads. The router already requires `read`, so these declare nothing.
	r.Get("/works", a.listWorks)
	r.Get("/works/{id}", a.getWork)
	r.Get("/editions/{id}", a.getEdition)
	r.Get("/assets", a.listAssets)
	r.Get("/assets/{id}", a.getAsset)
	r.Get("/libraries", a.listLibraries)
	r.Get("/libraries/{id}", a.getLibrary)
	r.Get("/blobs/{hash}", a.getBlob)
	r.Get("/blobs/{hash}/probe", a.getBlobProbe)
	r.Get("/peers", a.listPeers)
	r.Get("/replicas", a.listReplicas)
	r.Get("/devices", a.listDevices)
	r.Get("/devices/{id}", a.getDevice)
	r.Get("/providers", a.listProviders)
	r.Get("/quality-profiles", a.listQualityProfiles)
	r.Get("/quality-profiles/{id}", a.getQualityProfile)
	r.Get("/publications", a.listPublications)
	r.Get("/publications/{id}", a.getPublication)
	r.Get("/consumption/sessions", a.listSessions)
	r.Get("/consumption/sessions/{id}", a.getSession)
	r.Get("/jobs", a.listJobs)
	r.Get("/jobs/{id}", a.getJob)
	r.Get("/events", a.streamEvents)

	// Writes.
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/libraries", a.createLibrary)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/libraries/{id}/roots", a.createLibraryRoot)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/libraries/{id}/scan", a.scanLibrary)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Delete("/assets/{id}", a.deleteAsset)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/jobs/{id}/retry", a.retryJob)
	// Registering a device is a write, not an admin action: a television
	// announcing what it can play is ordinary client traffic, and requiring an
	// admin token for it would put an admin credential on every set-top box in
	// the house.
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/devices", a.registerDevice)
	// A quality profile is the standard desired state is measured against
	// (§62). Authoring one is a write rather than an admin action: it is
	// ordinary operator configuration, in the same class as creating a
	// library, not a credential operation.
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/quality-profiles", a.createQualityProfile)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Put("/quality-profiles/{id}", a.updateQualityProfile)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Delete("/quality-profiles/{id}", a.deleteQualityProfile)
	// Evaluating candidates writes NOTHING. It is a POST because the body
	// carries the candidates, not because it changes anything — the same
	// reasoning as /playback/plan — so `read` is enough, which the router
	// already requires. §63 says evaluation is inspectable, and an endpoint
	// that needed a write token would not be inspectable by anything holding a
	// read-only credential.
	r.Post("/quality-profiles/{id}/evaluate", a.evaluateCandidates)

	// Desired state (§55). Wanting is ordinary operator traffic, not an admin
	// action, so these need `write` rather than `admin`. Mounted as a group
	// because reads and writes belong to one resource and splitting them
	// across this file's two halves would make the scope contract harder to
	// read off, not easier.
	a.mountDesired(r)
	// Satisfaction explains §56's two axes for one want. Mounted only when a
	// catalog was supplied — see Options.Catalog.
	if a.catalog != nil {
		r.Get("/desired/{id}/satisfaction", a.getSatisfaction)
		// Asking for a reconciliation is a write: it queues work.
		r.With(httpapi.RequireScope(auth.ScopeWrite)).
			Post("/desired/{id}/reconcile", a.reconcileDesired)
	}
	// Consumption is ordinary client traffic like device registration: a
	// television reporting where it has reached needs a write token, not an
	// admin one.
	// Planning is a POST because the body carries the device and asset, not
	// because it changes anything: it opens no session and writes nothing. It
	// needs only `read`, which the router already requires.
	r.Post("/playback/plan", a.planPlayback)
	// Starting a playback opens a session and mints a credential, so it is a
	// write even though the bytes it points at are read-only.
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/playback", a.startPlayback)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/playback/remux", a.enqueueRemux)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/consumption/sessions", a.createSession)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).
		Post("/consumption/sessions/{id}/transitions", a.applyTransition)

	// Credentials are admin-only in both directions. A `write` token that could
	// mint itself a token is a `write` token that can become admin, and listing
	// them is a map of every way into the system.
	r.With(httpapi.RequireScope(auth.ScopeAdmin)).Get("/tokens", a.listTokens)
	r.With(httpapi.RequireScope(auth.ScopeAdmin)).Post("/tokens", a.createToken)
	r.With(httpapi.RequireScope(auth.ScopeAdmin)).Get("/tokens/{id}", a.getToken)
	r.With(httpapi.RequireScope(auth.ScopeAdmin)).Delete("/tokens/{id}", a.revokeToken)
}

// write renders a successful JSON response.
func (a *API) write(w http.ResponseWriter, r *http.Request, status int, body any) {
	buf, err := marshal(body)
	if err != nil {
		a.log.Error("encoding a response failed",
			"request_id", httpapi.RequestIDFrom(r.Context()), "path", r.URL.Path, "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// #nosec G705 -- the body is JSON produced by encoding/json and served as
	// application/json with nosniff; there is no HTML context to escape into.
	_, _ = w.Write(buf)
}

// fail maps an internal error onto a problem document.
//
// "No such row" is a 404 rather than a 500 because it is the ordinary "you
// asked for something that is not here" and must not be a page in an on-call
// runbook. Each layer spells it differently, so all three spellings are handled
// here rather than at every call site, where one would eventually be missed and
// a missing work would become an alert.
func (a *API) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, jobs.ErrNotFound) || errors.Is(err, auth.ErrNotFound) {
		httpapi.Fail(w, r, problem.NotFound("no "+what+" with that identifier"))
		return
	}
	a.log.Error("a request failed",
		"request_id", httpapi.RequestIDFrom(r.Context()),
		"path", r.URL.Path, "resource", what, "error", err)
	httpapi.Fail(w, r, problem.Internal())
}
