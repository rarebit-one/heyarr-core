package resources

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
	"github.com/rarebit-one/heyarr-core/internal/renderer"
)

// Driving a renderer (§68).
//
// # Why this is server-side and not in the CLI
//
// Both halves of it have to be, and for reasons that were measured rather than
// reasoned about:
//
//   - SSDP discovery is multicast on the local segment. It does not cross a
//     routed link, so a laptop on a VPN finds nothing at all.
//   - A Samsung's AllShare renderer refuses CONTROL from off its own subnet —
//     401 from a tunnel address, 200 from a host on the same /24 — while
//     happily fetching CONTENT from anywhere.
//
// So the thing issuing SOAP has to sit inside the house. Putting it in the CLI
// would make "play this in the living room" work only from a laptop that
// happens to be at home, and would die when the terminal closed. Here, one
// deployment serves the CLI, the MCP tools and anything else later: they are
// all clients of the same three endpoints.
//
// The content path is unaffected by any of this — the renderer fetches bytes
// straight from the peer, and the controller stays out of it (§32).

// rendererCacheTTL is how long a discovery result is reused.
//
// Discovery costs a multicast round and a description fetch per device, and
// it takes seconds — far too slow to run before every Pause. But a renderer's
// location is a DHCP lease, so caching it forever means driving a stale
// address after a reboot. A few minutes covers a session of pausing and
// scrubbing without outliving a lease.
const rendererCacheTTL = 5 * time.Minute

// discoveryTimeout bounds one SSDP sweep. Devices may wait up to three seconds
// before answering, so anything shorter systematically misses the slow ones.
const discoveryTimeout = 5 * time.Second

// rendererCache holds what discovery last found, keyed by UDN.
//
// UDN rather than address, because the address is a lease and the UDN is the
// device. A renderer that comes back on a different IP is the same renderer,
// and anything a client stored — a "play here by default" setting — must keep
// pointing at it.
type rendererCache struct {
	mu        sync.Mutex
	byUDN     map[string]renderer.Renderer
	refreshed time.Time
}

func (c *rendererCache) replace(found []renderer.Renderer, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byUDN == nil {
		c.byUDN = make(map[string]renderer.Renderer, len(found))
	}
	for _, r := range found {
		// Merged rather than replaced. A device that was asleep during this
		// sweep is not gone, and dropping it would make a paused film
		// un-resumable because the television dimmed its screen.
		c.byUDN[r.UDN] = r
	}
	c.refreshed = now
}

func (c *rendererCache) list() []renderer.Renderer {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]renderer.Renderer, 0, len(c.byUDN))
	for _, r := range c.byUDN {
		out = append(out, r)
	}
	return out
}

func (c *rendererCache) get(udn string) (renderer.Renderer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byUDN[udn]
	return r, ok
}

func (c *rendererCache) fresh(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.refreshed.IsZero() && now.Sub(c.refreshed) < rendererCacheTTL
}

// RendererView is the wire shape.
type RendererView struct {
	UDN          string `json:"udn"`
	Name         string `json:"name"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Location     string `json:"location"`
	// State is what the device is doing, when it was asked. Empty when it did
	// not answer — which is normal for a screen that has gone to standby, and
	// is reported rather than hidden.
	State string `json:"state,omitempty"`
}

// listRenderers answers GET /api/v1/renderers.
func (a *API) listRenderers(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "true"
	found, err := a.renderers(r.Context(), refresh)
	if err != nil {
		a.fail(w, r, "renderers", err)
		return
	}
	views := make([]RendererView, 0, len(found))
	for _, rend := range found {
		views = append(views, RendererView{
			UDN: rend.UDN, Name: rend.FriendlyName, Manufacturer: rend.Manufacturer,
			Model: rend.ModelName, Location: rend.Location,
		})
	}
	a.write(w, r, http.StatusOK, map[string]any{"renderers": views})
}

// renderers returns the known renderers, discovering when the cache is cold.
func (a *API) renderers(ctx context.Context, refresh bool) ([]renderer.Renderer, error) {
	if a.rendererCache == nil {
		return nil, errors.New("renderer control is not enabled on this node")
	}
	if !refresh && a.rendererCache.fresh(a.now()) {
		return a.rendererCache.list(), nil
	}
	sweep, cancel := context.WithTimeout(ctx, discoveryTimeout*2)
	defer cancel()

	found, problems := renderer.DiscoverRenderers(sweep, a.rendererClient,
		renderer.DiscoverOptions{Timeout: discoveryTimeout})
	for _, p := range problems {
		// Logged and not returned. One unreachable device must not hide the
		// others, and a sweep that found three renderers and failed to
		// describe a fourth has still answered the question.
		a.log.Warn("renderer discovery", "error", p)
	}
	a.rendererCache.replace(found, a.now())
	return a.rendererCache.list(), nil
}

// controllerFor resolves a UDN to something that can be driven.
func (a *API) controllerFor(ctx context.Context, udn string) (renderer.Renderer, *renderer.Controller, error) {
	if a.rendererCache == nil {
		return renderer.Renderer{}, nil, errors.New("renderer control is not enabled on this node")
	}
	rend, ok := a.rendererCache.get(udn)
	if !ok {
		// One rediscovery before giving up. The common case is a controller
		// that restarted, or a client holding a UDN from yesterday, and
		// failing that with "unknown renderer" would be true and useless.
		if _, err := a.renderers(ctx, true); err != nil {
			return renderer.Renderer{}, nil, err
		}
		if rend, ok = a.rendererCache.get(udn); !ok {
			return renderer.Renderer{}, nil, errNoSuchRenderer
		}
	}
	ctrl, err := renderer.NewController(a.rendererClient, rend)
	return rend, ctrl, err
}

var errNoSuchRenderer = errors.New("no renderer with that id answered; it may be switched off")

// playOnRendererRequest is the POST /renderers/{udn}/play body.
type playOnRendererRequest struct {
	AssetID string `json:"asset_id"`
}

// playOnRenderer plans a playback, mints a capability URL and pushes it.
//
// It is deliberately ONE call. The three steps — plan, mint, push — always
// happen together, and a client made to orchestrate them would be a client
// that can get the order wrong, hold a capability it never uses, or open a
// session for a playback it never started.
func (a *API) playOnRenderer(w http.ResponseWriter, r *http.Request) {
	var body playOnRendererRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("asset_id", body.AssetID); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	udn := chi.URLParam(r, "udn")

	rend, ctrl, err := a.controllerFor(r.Context(), udn)
	if err != nil {
		a.failRenderer(w, r, err)
		return
	}

	// The renderer's own declared capabilities become the device profile the
	// planner decides against, so a plan for a television is judged on what
	// that television said about itself rather than on what somebody typed in.
	profile, err := renderer.FetchProfile(r.Context(), a.rendererClient, rend)
	if err != nil {
		// Not fatal. A renderer that will not answer GetProtocolInfo can still
		// be played to; the planner simply has nothing to check against and
		// says so in its reasons (device_declares_nothing).
		a.log.Warn("reading a renderer's capabilities",
			"request_id", httpapi.RequestIDFrom(r.Context()), "udn", udn, "error", err)
	}

	started, err := a.startForRendererCtx(r.Context(), body.AssetID, udn, rend, profile)
	if err != nil {
		a.failRenderer(w, r, err)
		return
	}
	if started.RenderURL == "" {
		httpapi.Fail(w, r, problem.New(http.StatusConflict, problem.TypeConflict,
			"Nothing to play from",
			cmpOr(started.RenderUnavailable, "this peer cannot serve these bytes to a renderer")))
		return
	}

	if err := ctrl.Start(r.Context(), started.RenderURL, started.Title, started.MIME); err != nil {
		// The renderer refused. That is the device's answer and belongs to the
		// caller verbatim — "714 Illegal MIME-type" is a different problem
		// from "701 Transition not available" and they need different fixes.
		httpapi.Fail(w, r, problem.New(http.StatusBadGateway, problem.TypeConflict,
			"The renderer refused", err.Error()))
		return
	}

	a.write(w, r, http.StatusOK, map[string]any{
		"session_id":   started.SessionID,
		"renderer":     RendererView{UDN: rend.UDN, Name: rend.FriendlyName, Model: rend.ModelName},
		"asset_id":     body.AssetID,
		"title":        started.Title,
		"decision":     started.Decision,
		"content_type": started.MIME,
	})
}

// transportAction applies one transport verb.
func (a *API) transportAction(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, ctrl, err := a.controllerFor(r.Context(), chi.URLParam(r, "udn"))
		if err != nil {
			a.failRenderer(w, r, err)
			return
		}
		switch verb {
		case "pause":
			err = ctrl.Pause(r.Context())
		case "resume":
			// Resume is Play. UPnP has no separate verb: Play from
			// PAUSED_PLAYBACK continues, and Play from STOPPED restarts.
			err = ctrl.Play(r.Context())
		case "stop":
			err = ctrl.Stop(r.Context())
		default:
			httpapi.Fail(w, r, problem.BadRequest("unknown transport action "+verb))
			return
		}
		if err != nil {
			httpapi.Fail(w, r, problem.New(http.StatusBadGateway, problem.TypeConflict,
				"The renderer refused", err.Error()))
			return
		}
		a.rendererStatus(w, r)
	}
}

// seekRequest is the POST /renderers/{udn}/seek body.
type seekRequest struct {
	// Seconds is an absolute offset from the start, not a delta. UPnP's
	// REL_TIME means the same thing despite its name, and a field called
	// "offset" would invite the other reading.
	Seconds float64 `json:"seconds"`
}

func (a *API) seekRenderer(w http.ResponseWriter, r *http.Request) {
	var body seekRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	_, ctrl, err := a.controllerFor(r.Context(), chi.URLParam(r, "udn"))
	if err != nil {
		a.failRenderer(w, r, err)
		return
	}
	if err := ctrl.Seek(r.Context(), time.Duration(body.Seconds*float64(time.Second))); err != nil {
		httpapi.Fail(w, r, problem.New(http.StatusBadGateway, problem.TypeConflict,
			"The renderer refused", err.Error()))
		return
	}
	a.rendererStatus(w, r)
}

// rendererStatus answers GET /api/v1/renderers/{udn}/status.
func (a *API) rendererStatus(w http.ResponseWriter, r *http.Request) {
	rend, ctrl, err := a.controllerFor(r.Context(), chi.URLParam(r, "udn"))
	if err != nil {
		a.failRenderer(w, r, err)
		return
	}
	state, err := ctrl.State(r.Context())
	if err != nil {
		httpapi.Fail(w, r, problem.New(http.StatusBadGateway, problem.TypeConflict,
			"The renderer did not answer", err.Error()))
		return
	}
	out := map[string]any{
		"renderer": RendererView{
			UDN: rend.UDN, Name: rend.FriendlyName, Model: rend.ModelName,
			State: string(state),
		},
		"state":   string(state),
		"playing": state.Playing(),
	}
	// Position is asked for but not required. A renderer answers
	// NOT_IMPLEMENTED for it legitimately, and a status call that failed
	// because of that would be a status call that fails on real devices.
	if pos, err := ctrl.Position(r.Context()); err == nil {
		out["elapsed_seconds"] = pos.Elapsed.Seconds()
		if pos.Duration > 0 {
			out["duration_seconds"] = pos.Duration.Seconds()
		}
	}
	a.write(w, r, http.StatusOK, out)
}

func (a *API) failRenderer(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errNoSuchRenderer) {
		httpapi.Fail(w, r, problem.NotFound(err.Error()))
		return
	}
	a.fail(w, r, "renderers", err)
}

// startedPlayback is what playOnRenderer needs out of the playback lane.
type startedPlayback struct {
	SessionID         string
	RenderURL         string
	RenderUnavailable string
	Title             string
	MIME              string
	Decision          string
}

// startForRenderer registers the renderer as a Device and begins a playback
// against it.
//
// The device registration is the interesting half. A Device (§68) is a
// capability profile the planner decides against, and until now somebody had
// to type one in. A renderer just told us what it can play, so the profile is
// upserted from the device's own answer, keyed by its UDN — which survives a
// DHCP lease change, where the address does not.
//
// That closes the loop the first commit in this branch opened: the television
// declares its codecs, the planner judges the file against them, and nobody
// hand-maintains a list.
func (a *API) startForRendererCtx(ctx context.Context, assetID, udn string, rend renderer.Renderer, profile playback.DeviceProfile) (startedPlayback, error) {
	deviceID, err := a.upsertRendererDevice(ctx, udn, rend, profile)
	if err != nil {
		return startedPlayback{}, err
	}

	// Verb is left empty so the playback lane resolves it from the media:
	// watch when there is video, listen when there is not. A renderer is not
	// the right place to decide that — the Devialet has no screen, and the
	// asset already knows whether it has a picture.
	started, prob := a.beginPlayback(ctx, assetID, deviceID, "")
	if prob != nil {
		return startedPlayback{}, prob
	}

	title, mime := a.assetTitle(ctx, assetID)
	return startedPlayback{
		SessionID:         started.SessionID,
		RenderURL:         started.RenderURL,
		RenderUnavailable: started.RenderUnavailable,
		Title:             title,
		MIME:              mime,
		Decision:          started.Decision,
	}, nil
}

// upsertRendererDevice records a discovered renderer as a Device, returning its id.
//
// It reuses upsertDevice rather than writing its own SQL: that function owns
// how a profile is encoded, and a second writer would be a second encoding to
// keep in step.
func (a *API) upsertRendererDevice(ctx context.Context, udn string, rend renderer.Renderer, profile playback.DeviceProfile) (string, error) {
	var id string
	err := a.db.InTx(ctx, func(tx *sql.Tx) error {
		existing, err := deviceByKey(ctx, tx, udn)
		switch {
		case err == nil:
			id = existing.ID
		case errors.Is(err, sql.ErrNoRows):
			id = a.newID()
		default:
			return err
		}

		now := a.now().UTC()
		device := Device{
			ID: id, DeviceKey: udn, Name: rend.FriendlyName, Platform: rendererPlatform,
			CreatedAt: now, UpdatedAt: now, LastSeenAt: now,
			Profile: DeviceProfile{
				Containers:  profile.Containers,
				VideoCodecs: profile.VideoCodecs,
				AudioCodecs: profile.AudioCodecs,
			},
		}
		// A renderer that would not answer GetProtocolInfo must not blank out
		// a profile that was read successfully last time. Keeping the old one
		// is right for the same reason the discovery cache merges rather than
		// replaces: a device being briefly unhelpful is not a device that has
		// forgotten what it can play.
		if !profile.Declares() && err == nil {
			device.Profile = existing.Profile
		}
		if err == nil {
			device.CreatedAt = existing.CreatedAt
		}
		return upsertDevice(ctx, tx, device)
	})
	return id, err
}

// rendererPlatform marks a Device that discovery created rather than a client.
//
// It matters for provenance: a hand-registered device is somebody's claim and
// must not be overwritten, while one of these is a reading that should be
// refreshed. Nothing consumes it yet; recording it now is cheaper than
// inferring it later.
const rendererPlatform = "dlna-renderer"

// cmpOr returns the first non-empty string.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// assetTitle reads a human name for an asset, for the renderer's on-screen
// display. It falls back to the filename, which is always present, rather than
// failing: a missing title is a worse label, not a reason not to play.
func (a *API) assetTitle(ctx context.Context, assetID string) (title, mime string) {
	var (
		filename sql.NullString
		workName sql.NullString
		m        sql.NullString
	)
	_ = a.reader.QueryRowContext(ctx, `
		SELECT a.filename, a.mime, w.title
		FROM assets a
		LEFT JOIN editions e ON e.id = a.edition_id
		LEFT JOIN works w ON w.id = e.work_id
		WHERE a.id = ?`, assetID).Scan(&filename, &m, &workName)
	switch {
	case workName.Valid && workName.String != "":
		title = workName.String
	case filename.Valid:
		title = filename.String
	}
	return title, m.String
}

// ---------------------------------------------------------------------------
// The same three operations, without HTTP.
//
// The MCP tools (§71) call these directly rather than through the router,
// which is the point of ADR-0019's "one server, two front doors": a tool and a
// REST call must not be able to do different things, and they cannot if there
// is one implementation. The handlers above are thin wrappers over exactly
// these.
// ---------------------------------------------------------------------------

// RenderersFor lists what can be played to.
func (a *API) RenderersFor(ctx context.Context, refresh bool) ([]RendererView, error) {
	found, err := a.renderers(ctx, refresh)
	if err != nil {
		return nil, err
	}
	views := make([]RendererView, 0, len(found))
	for _, rend := range found {
		views = append(views, RendererView{
			UDN: rend.UDN, Name: rend.FriendlyName, Manufacturer: rend.Manufacturer,
			Model: rend.ModelName, Location: rend.Location,
		})
	}
	return views, nil
}

// PlayOnRenderer plans, mints and pushes.
func (a *API) PlayOnRenderer(ctx context.Context, udn, assetID string) (map[string]any, error) {
	rend, ctrl, err := a.controllerFor(ctx, udn)
	if err != nil {
		return nil, err
	}
	profile, err := renderer.FetchProfile(ctx, a.rendererClient, rend)
	if err != nil {
		a.log.Warn("reading a renderer's capabilities", "udn", udn, "error", err)
	}
	started, err := a.startForRendererCtx(ctx, assetID, udn, rend, profile)
	if err != nil {
		return nil, err
	}
	if started.RenderURL == "" {
		return nil, errors.New(cmpOr(started.RenderUnavailable,
			"this peer cannot serve these bytes to a renderer"))
	}
	if err := ctrl.Start(ctx, started.RenderURL, started.Title, started.MIME); err != nil {
		return nil, err
	}
	return map[string]any{
		"playing":    started.Title,
		"on":         rend.FriendlyName,
		"session_id": started.SessionID,
		"decision":   started.Decision,
	}, nil
}

// ControlRenderer applies pause, resume or stop.
func (a *API) ControlRenderer(ctx context.Context, udn, action string) (map[string]any, error) {
	_, ctrl, err := a.controllerFor(ctx, udn)
	if err != nil {
		return nil, err
	}
	switch action {
	case "pause":
		err = ctrl.Pause(ctx)
	case "resume":
		err = ctrl.Play(ctx)
	case "stop":
		err = ctrl.Stop(ctx)
	default:
		return nil, errors.New("unknown action " + action)
	}
	if err != nil {
		return nil, err
	}
	return a.RendererStatusFor(ctx, udn)
}

// RendererStatusFor reports what a renderer is doing.
func (a *API) RendererStatusFor(ctx context.Context, udn string) (map[string]any, error) {
	rend, ctrl, err := a.controllerFor(ctx, udn)
	if err != nil {
		return nil, err
	}
	state, err := ctrl.State(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"renderer": rend.FriendlyName,
		"state":    string(state),
		"playing":  state.Playing(),
	}
	if pos, err := ctrl.Position(ctx); err == nil {
		out["elapsed_seconds"] = pos.Elapsed.Seconds()
		if pos.Duration > 0 {
			out["duration_seconds"] = pos.Duration.Seconds()
		}
	}
	return out, nil
}
