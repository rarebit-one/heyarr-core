// Package controller hosts the API, the job scheduler and the reconcilers. It
// owns coordinated mutable decisions and never routinely moves bulk content
// bytes (spec §7).
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/mcp"
	personalstateapi "github.com/rarebit-one/heyarr-core/internal/api/personalstate"
	"github.com/rarebit-one/heyarr-core/internal/api/render"
	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/downloads"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/indexers"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media"
	"github.com/rarebit-one/heyarr-core/internal/pairrelay"
	"github.com/rarebit-one/heyarr-core/internal/peer/health"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	psstore "github.com/rarebit-one/heyarr-core/internal/personalstate/store"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// Controller is the control-plane role.
type Controller struct {
	cfg config.Config
	log *slog.Logger
	db  *sqlite.DB
}

// New constructs the controller. It must not start anything; Run does that, so
// that construction failures are reported before any listener exists.
func New(cfg config.Config, log *slog.Logger) *Controller {
	return &Controller{cfg: cfg, log: log.With("role", "controller")}
}

// Name identifies the role in logs and supervision.
func (c *Controller) Name() string { return "controller" }

// startupTimeout bounds the work that must finish even if shutdown is
// requested — opening the database and migrating it. It exists so that a
// wedged migration cannot make the process unkillable.
const startupTimeout = 5 * time.Minute

// shutdownTimeout bounds draining in-flight HTTP requests. It sits inside
// cli.DefaultShutdownGrace so that the server gives up before the supervisor
// does — otherwise the supervisor's message ("roles did not stop") replaces the
// server's, which is the one that says what was still in flight.
const shutdownTimeout = 10 * time.Second

// Run blocks until ctx is cancelled, then shuts down cleanly.
//
// Milestone 1 fills this in: the resource API (M1-14), the job scheduler
// (M1-05) and the reconcilers mount onto the HTTP foundation started here.
func (c *Controller) Run(ctx context.Context) error {
	// Startup deliberately does NOT use the shutdown context.
	//
	// A SIGTERM arriving while a migration is in flight would otherwise cancel
	// it mid-statement. Transactional DDL means that is safe rather than
	// corrupting, but it turns an ordinary restart into a startup *error* and
	// makes the next start redo the work — and a service you are afraid to
	// restart during an upgrade is worse than one that takes a moment longer to
	// stop. So schema work runs to completion, bounded by a timeout so a wedged
	// migration cannot make the process unkillable.
	startupCtx, cancelStartup := context.WithTimeout(context.WithoutCancel(ctx), startupTimeout)
	defer cancelStartup()

	// The controller is the only role that opens the database for writing: it
	// owns coordinated mutable state (§7, ADR-0003). Workers and peers reach it
	// through the controller, never by opening the file themselves.
	db, err := sqlite.Open(startupCtx, sqlite.Options{Path: c.cfg.Database.Path, Logger: c.log})
	if err != nil {
		return fmt.Errorf("controller: opening database: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			c.log.Error("closing database", "error", err)
		}
	}()

	if err := sqlite.Migrate(startupCtx, db); err != nil {
		return fmt.Errorf("controller: migrating database: %w", err)
	}
	version, err := sqlite.SchemaVersion(startupCtx, db)
	if err != nil {
		return fmt.Errorf("controller: reading schema version: %w", err)
	}

	// The libraries block is control-plane configuration, so the controller
	// owns turning it into rows — and the scan that follows is a job, because
	// the worker that runs it may be another process entirely (§4, ADR-0002).
	//
	// Like the migration above it, this runs on the STARTUP context rather than
	// the shutdown one, and for the same reason: it is idempotent schema-shaped
	// work that the next start would only have to redo, and a SIGTERM arriving
	// here should be an ordinary stop rather than a start that half-happened.
	// A misconfigured library is a startup failure — discovering it hours later
	// when a scan silently never happens is much more expensive.
	if err := reconcileLibraries(startupCtx, db, c.cfg, c.log); err != nil {
		return err
	}

	// Quality profiles are seeded before anything else can want them (§62,
	// M3-01). A Heyarr with no profiles is one where the first interesting
	// thing you can do — want something — requires authoring JSON against a
	// vocabulary you have not read.
	//
	// It converges on the profile name and never overwrites, so an operator
	// who tunes a default keeps their tuning across every restart.
	if err := seedQualityProfiles(startupCtx, db, c.cfg, c.log); err != nil {
		return err
	}

	// Shutdown may have been requested while the schema work ran. That is a
	// clean stop, not a failure — report it as one.
	if ctx.Err() != nil {
		c.log.Info("controller stopped during startup", "schema_version", version)
		return nil
	}

	c.db = db

	// The CAS root is the controller's to have ready before it says it is up:
	// /readyz reports on it, and an operator who has not created it yet should
	// learn that from a readiness probe rather than from an ingest failing
	// hours into a scan. The layout inside it belongs to the storage fabric
	// (ADR-0006); all that happens here is that the directory exists.
	if c.cfg.CAS.Root != "" {
		if err := os.MkdirAll(c.cfg.CAS.Root, 0o750); err != nil {
			return fmt.Errorf("controller: creating the CAS root %s: %w", c.cfg.CAS.Root, err)
		}
	}

	// The CAS is opened once, here, and handed to whatever serves from it. The
	// controller does not know the layout inside the root (ADR-0006) — only
	// that a store exists to read blobs through.
	blobStore, err := cas.OpenFS(c.cfg.CAS.Root)
	if err != nil {
		return fmt.Errorf("controller: opening the CAS at %s: %w", c.cfg.CAS.Root, err)
	}

	// The peer identity, and ADR-0010's refusal.
	//
	// This runs BEFORE the server is constructed, let alone started. A node
	// that binds a listener and then discovers its identity is contested has
	// already served reads under it, and "the read was served by peer X" is a
	// claim that cannot be withdrawn once another machine has made it too.
	//
	// It is also before the reconcilers and the job queue: a job leased under
	// a contested identity is worse again, because the lease outlives the
	// process that took it.
	identityEvents, err := events.New(events.Options{
		Writer: db.Writer(), Reader: db.Reader(), Logger: c.log,
	})
	if err != nil {
		return fmt.Errorf("controller: opening the event log for the peer identity: %w", err)
	}
	identityCatalog, err := catalog.New(catalog.Options{
		DB: db, Events: identityEvents,
		PeerName: c.cfg.Peer.Name, PeerSite: c.cfg.Peer.Site, Logger: c.log,
	})
	if err != nil {
		return fmt.Errorf("controller: opening the catalog for the peer identity: %w", err)
	}
	self, err := identity.Ensure(startupCtx, identity.Options{
		DataDir: c.cfg.DataDir,
		Peers:   identityCatalog,
		CAS:     blobStore,
		Logger:  c.log,
	})
	if err != nil {
		return fmt.Errorf("controller: %w", err)
	}

	// This node's certificate material, derived once and used twice: the peer
	// listener presents it, and the health probe dials with it (#184). It is
	// built here, before either, so that an identity that cannot produce a
	// certificate is a startup failure rather than a probe that quietly never
	// works.
	material, err := c.peerMaterial(self)
	if err != nil {
		return err
	}

	// Peer reachability (§31, §32, M4-10, #184).
	//
	// Constructed BEFORE both servers, because both record liveness through
	// it: an inbound request from a peer is the best evidence that peer is up,
	// and it is evidence that arrives on the request path — on the client API
	// for anything holding a bearer token, and on the PEER surface for a
	// remote peer, which holds none.
	//
	// The idle probe dials the peer fabric itself, pinned, with this node's
	// certificate. It used to speak plain HTTPS to /healthz, which an mTLS
	// listener will not complete a handshake with — so in the topology this
	// milestone actually builds, the probe could not answer either, and a
	// remote peer's health never left `unknown` (#184).
	peerHealth, err := health.New(health.Options{
		DB:     db,
		Events: identityEvents,
		Prober: health.MTLSProber{Material: material, Logger: c.log},
		Logger: c.log,
	})
	if err != nil {
		return fmt.Errorf("controller: opening peer health: %w", err)
	}

	srv, members, err := c.newServer(ctx, db, blobStore, version, peerHealth, self.PeerID)
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("controller: %w", err)
	}

	// The peer fabric's listener (§26, ADR-0012, M4-05). It is constructed
	// unconditionally — which proves the identity on disk can still produce a
	// certificate — and binds only when peer.listen is set. A single-node
	// deployment needs no certificate configuration at all.
	peerSrv, err := c.newPeerSurface(db, self, members, blobStore, material, peerHealth)
	if err != nil {
		return err
	}
	if err := peerSrv.Start(); err != nil {
		return err
	}

	// Reconciliation runs on the SERVING context, not the startup one: it is
	// ongoing work rather than schema-shaped setup, and it must stop when the
	// controller does.
	reconcileEvents, err := events.New(events.Options{
		Writer: db.Writer(), Reader: db.Reader(), Logger: c.log,
	})
	if err != nil {
		return fmt.Errorf("controller: opening the event log for reconciliation: %w", err)
	}
	reconcileQueue, err := jobs.New(jobs.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: reconcileEvents,
	})
	if err != nil {
		return fmt.Errorf("controller: opening the job queue for reconciliation: %w", err)
	}
	startReconciliation(ctx, reconcileQueue, peerHealth, c.log)
	startUpgradeScan(ctx, reconcileQueue, c.log)
	// The provider health beat (#164). Same queue and the same serving
	// context: it is ongoing work and it must stop when the controller does.
	// See healthbeat.go for why a minute, why it runs on a degraded node, and
	// why its interval is also the capabilities cache's refresh rate.
	startProviderHealth(ctx, reconcileQueue, c.log)

	// The search beat (#130). It needs a catalog as well as a queue — unlike
	// the two sweeps above, it asks a per-want question before enqueueing
	// anything — and it shares this event log so the job transitions it causes
	// land in the same stream as everything else (§76, ADR-0009).
	beatCatalog, err := catalog.New(catalog.Options{
		DB: db, Events: reconcileEvents,
		PeerName: c.cfg.Peer.Name, PeerSite: c.cfg.Peer.Site, Logger: c.log,
	})
	if err != nil {
		return fmt.Errorf("controller: opening the catalog for the search beat: %w", err)
	}
	startSearchBeat(ctx, beatCatalog, reconcileQueue, c.log)

	// The download poll beat (#247). Same queue and the same serving context.
	// See downloadbeat.go for why fifteen seconds rather than the health
	// beat's minute, why the startup pass is the important one, and why this
	// beat asks the configuration first where the health beat does not.
	startDownloadPoll(ctx, c.cfg.Providers, reconcileQueue, c.log)

	// The continuous control-plane backup (§49, ADR-0044, M7-02). Its interval
	// was validated at config load, so the error here cannot fire; it is read
	// rather than dropped so a future change to BackupInterval cannot silently
	// pass an unparsed value through.
	backupInterval, _ := c.cfg.BackupInterval()
	startBackup(ctx, db, reconcileEvents, c.cfg.DataDir, c.cfg.Backup.Dir,
		backupInterval, self.PeerID, c.log, material, members)

	// "started" is logged only after every listener is bound. A start line
	// printed before the socket exists is a lie that costs someone an
	// afternoon: the supervisor, the acceptance script and an operator tailing
	// the log all treat it as "you can talk to it now".
	c.log.Info("controller started",
		"database", c.cfg.Database.Path,
		"schema_version", version,
		"peer_id", self.PeerID,
		// The PUBLIC key. It is what another site needs in order to enrol this
		// node (ADR-0012), and it is safe in a log by construction — the
		// private half is a file this process never reads into a log field.
		"peer_public_key", self.PublicKeyString(),
		"http_addr", srv.Addr(),
		"unix_socket", srv.SocketPath(),
		"auth_enabled", c.cfg.HTTP.Auth.Enabled,
		// Empty when this node serves no peer surface, which is a
		// configuration rather than a failure.
		"peer_addr", peerSrv.Addr())

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-srv.Err():
		runErr = err
	case err := <-peerSrv.Err():
		runErr = err
	}

	// Draining must not use the cancelled context, or shutdown would return
	// instantly and kill every in-flight request — including a range response
	// halfway through a large blob, which is precisely the request most worth
	// finishing.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		c.log.Error("the http server did not shut down cleanly", "error", err)
	}
	if err := peerSrv.Shutdown(shutdownCtx); err != nil {
		c.log.Error("the peer surface did not shut down cleanly", "error", err)
	}

	c.log.Info("controller stopped")
	return runErr
}

// newServer wires the HTTP API. Authentication is constructed even when it is
// disabled, so the two configurations differ in one boolean rather than in
// which objects exist.
// It returns the membership store alongside the server because the peer
// surface (M4-05) must consult the SAME trust root the enrolment endpoints
// write and the request guard reads. Two stores over one database would answer
// identically today and would be exactly the seam a cache gets added behind.
// The health tracker is passed IN rather than built here for the mirror-image
// reason: it is constructed before the server so that the peer guard mounted
// on this router can record liveness through the same tracker the idle probe
// and the reconciler read.
func (c *Controller) newServer(ctx context.Context, db *sqlite.DB, blobStore cas.Store, schemaVersion int64, peerHealth *health.Tracker, selfPeerID string) (*httpapi.Server, *membership.Store, error) {
	store, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{Store: store})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}
	// Resolved here so a misconfigured path stops startup rather than being
	// discovered later, and so GET /api/v1/system can report what this node
	// can actually do (ADR-0023). An absent toolchain is not an error.
	toolchain, err := media.Resolve(context.Background(), media.Options{
		FFprobePath: c.cfg.Media.FFprobePath,
		FFmpegPath:  c.cfg.Media.FFmpegPath,
		Logger:      c.log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}

	// The log is built here rather than inside mounts because it now has two
	// consumers: the resource API, which appends to it, and GET /api/v1/system,
	// which reports its head. One log, so the head the endpoint reports is the
	// head the stream resumes from.
	eventLog, err := events.New(events.Options{
		Writer: db.Writer(), Reader: db.Reader(), Logger: c.log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}
	// The peer fabric's trust root (§26, ADR-0012, M4-04). One store, shared
	// between the enrolment endpoints that write it and the request guard that
	// reads it, so that a peer removed through the API stops being trusted by
	// the very next request rather than by the next restart.
	members, err := membership.New(membership.Options{
		DB: db, Events: eventLog, Logger: c.log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: opening peer membership: %w", err)
	}
	// The Milestone 8 device-identity store (§40, ADR-0048): pinned user
	// identities and the device keys they vouch for. One store, shared by the
	// enrolment paths that write it and the authentication middleware that reads
	// it, so a device revoked through the API stops authenticating on the very
	// next request — the same "one trust root, read every request" shape as peer
	// membership above.
	deviceIdentities, err := deviceauth.New(deviceauth.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: eventLog,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: opening device identity store: %w", err)
	}
	mounts, publicMounts, err := c.mounts(ctx, db, store, blobStore, eventLog, members, deviceIdentities, selfPeerID)
	if err != nil {
		return nil, nil, err
	}
	// What this binary knows how to migrate to, as opposed to what the database
	// is actually at. The two are compared on GET /api/v1/system (#150), and
	// they are read from two different places on purpose: one is compiled in,
	// the other is on disk, and the whole point is to notice when they disagree.
	knownSchema, err := sqlite.KnownSchemaVersion()
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}

	srv, err := httpapi.New(httpapi.Options{
		Config:             c.cfg,
		Logger:             c.log,
		DB:                 db,
		Verifier:           verifier,
		DeviceVerifier:     deviceIdentities,
		Events:             eventLog,
		Media:              mediaInfo(toolchain),
		Build:              buildinfo.Get(),
		SchemaVersion:      schemaVersion,
		KnownSchemaVersion: knownSchema,
		CASRoot:            c.cfg.CAS.Root,
		Mount:              mounts,
		MountPublic:        publicMounts,
		PeerMembership:     members,
		PeerLiveness:       liveness(peerHealth),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}
	return srv, members, nil
}

// mounts is the API surface this controller serves.
//
// It is one function rather than a literal inside newServer because the
// OpenAPI parity test (ADR-0015) drives *this* list. A test that built its own
// list of mounts would be asserting that the specification matches a second
// hand-maintained list, which is the failure the ADR exists to prevent.
// The event log is passed in rather than built here because the queue records
// its own transitions through it (§76, ADR-0009) and GET /api/v1/system reports
// its head — and those must be the same log.
// mounts builds the authenticated API routes and, separately, the
// unauthenticated renderer route (ADR-0040). They are returned apart rather
// than in one slice because they are mounted on different routers with
// different trust roots, and a mix-up in either direction is severe: an API
// route mounted publicly is the library given away, and the renderer route
// mounted privately is a 401 for every television.
func (c *Controller) mounts(ctx context.Context, db *sqlite.DB, store *auth.Store, blobStore cas.Store, eventLog *events.Log, members *membership.Store, identities *deviceauth.Store, selfPeerID string) (apiMounts, publicMounts []httpapi.MountFunc, err error) {
	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}
	// The catalog answers §56's satisfaction questions for the API. It is the
	// same construction reconcileLibraries and the worker use — one catalog
	// per process would be tidier, and is a refactor rather than this issue.
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog,
		PeerName: c.cfg.Peer.Name, PeerSite: c.cfg.Peer.Site, Logger: c.log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: opening the catalog: %w", err)
	}
	// The provider registry, from configuration. Validation already happened
	// at config load — a malformed endpoint or a missing credential stopped
	// this process before it opened a database — so this cannot fail for a
	// reason an operator can act on.
	resolvedProviders, err := providers.Validate(c.cfg.Providers)
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}
	// ADR-0014, at the other end of the ladder. Checked before the registry is
	// built so a refusal names the configuration rather than arriving after a
	// provider has been registered and reported.
	if err := checkDownloadPaths(c.cfg, resolvedProviders, c.log); err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}
	providerRegistry, err := providers.BuildWith(resolvedProviders, c.log, nil,
		providers.Chain(indexers.Constructor, downloads.Constructor))
	if err != nil {
		return nil, nil, fmt.Errorf("controller: building the provider registry: %w", err)
	}

	// The renderer capability secret (ADR-0040), read before the resource API
	// because that is what mints the URLs.
	secret, err := render.EnsureSecret(c.cfg.DataDir)
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}

	api, err := resources.New(resources.Options{
		DB:         db,
		Jobs:       queue,
		Events:     eventLog,
		Tokens:     store,
		Catalog:    cat,
		Providers:  providerRegistry,
		Membership: members,
		Identities: identities,
		Logger:     c.log,

		RenderSecret:  secret,
		RenderBaseURL: renderBaseURL(c.cfg),
		SelfPeerID:    selfPeerID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}
	// The progress producer (§68, ADR-0024). It is what finally emits
	// session transitions: until now the consumption state machine had no
	// producer at all, so every session stayed at "created".
	api.StartProgressBeat(ctx)

	blobHandler, err := blobs.New(blobs.Options{Store: blobStore, Logger: c.log})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}

	// MCP mounts on the SAME authenticated router (§71, ADR-0019), so it
	// inherits the middleware chain, the request correlation and the `read`
	// scope floor rather than standing up a second server that would have to
	// re-earn all three. Its per-tool scope check turns that floor into a
	// contract.
	//
	// It is handed the resource API rather than the database for the write
	// intents: wanting content is one intent with two front doors, and two
	// implementations of it would drift silently.
	mcpServer, err := mcp.New(mcp.Options{
		DB:        db,
		Resources: api,
		Jobs:      queue,
		Logger:    c.log,
		Version:   buildinfo.Get().Version,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}

	// The renderer route (ADR-0040). Its signing secret belongs to the node
	// that serves the bytes, so a capability is only valid here — which is why
	// this milestone mints one only for a replica on this node.
	renderHandler, err := render.New(render.Options{
		Blobs:  blobHandler,
		Secret: secret,
		Logger: c.log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}

	// The device-pairing relay (§40, ADR-0022, ADR-0038). A dumb, ephemeral,
	// in-memory store-and-forward that two devices exchange through so an old
	// one can authorise a new one — mounted publicly, like the renderer route,
	// because a device being paired has no credential and the relay grants no
	// authority (see internal/pairrelay).
	relayHandler := pairrelay.NewHandler(pairrelay.HandlerOptions{Logger: c.log})

	// The encrypted personal-state plane's device-facing API (§38, §42,
	// ADR-0049). It stores the opaque things a device pushes — a space, the
	// wrapped copies of its key, the encrypted changes — and can read none of
	// them (Invariant 6). The store is a thin single-writer wrapper over the same
	// controller database (ADR-0003); the peer-to-peer sync surface (#322) opens
	// its own over the same DB, which is safe because there is one writer.
	psStore, err := psstore.New(psstore.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: opening the personal-state store: %w", err)
	}
	psAPI, err := personalstateapi.New(personalstateapi.Options{Store: psStore, Logger: c.log})
	if err != nil {
		return nil, nil, fmt.Errorf("controller: %w", err)
	}

	return []httpapi.MountFunc{api.Mount, blobHandler.Mount, mcpServer.Mount, psAPI.Mount},
		[]httpapi.MountFunc{renderHandler.Mount, relayHandler.Mount}, nil
}

// liveness converts a possibly-absent tracker into the interface the HTTP
// foundation takes.
//
// It exists because a typed nil pointer stored in an interface is not a nil
// interface: assigning a nil *health.Tracker straight to the field would give
// the guard something that reads as present and panics on first use. The
// conversion is one line and the bug is a nil dereference on the request path,
// so it is one line worth having.
func liveness(t *health.Tracker) httpapi.PeerLiveness {
	if t == nil {
		return nil
	}
	return t
}

// mediaInfo renders a resolved toolchain for GET /api/v1/system.
//
// The mapping lives here rather than in internal/media so that the media
// package stays free of the API's wire shape, and in the controller rather
// than in the HTTP package so that the HTTP foundation does not import the
// toolchain to describe it.
func mediaInfo(tc media.Toolchain) []httpapi.ToolInfo {
	tools := tc.Tools()
	out := make([]httpapi.ToolInfo, 0, len(tools))
	for _, t := range tools {
		out = append(out, httpapi.ToolInfo{
			Name:      t.Name,
			Path:      t.Path,
			Version:   t.Version,
			Available: t.Available,
			Detail:    t.Detail,
		})
	}
	return out
}
