package resources

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/api/render"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
	"github.com/rarebit-one/heyarr-core/internal/domain/routing"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Direct playback (§68, §32).
//
// POST /api/v1/playback plans, opens a session, and returns somewhere to play
// from. It is the one call a client makes when someone presses play.
//
// # The controller stays out of the content data path (§32)
//
// It returns a DIRECT URL to the peer holding the replica; it does not proxy
// bytes. That matters more than it sounds: a controller that proxies is a
// controller whose availability becomes playback's availability, which is
// exactly the coupling §53's degraded-operation model exists to avoid. A Full
// Peer must keep streaming when the controller has gone.
//
// # The URL is the ordinary blob endpoint, unchanged
//
// ADR-0013 is one endpoint with several consumers, and its consequences
// section forbids exactly what would be convenient here: "adding a
// player-shaped session token". So playback does not get its own byte route,
// its own auth scheme, or a signed URL — it gets a short-lived credential and
// the same endpoint replication and the web-seed use.
//
// The cost of honouring that is real and is stated rather than hidden: the
// credential is scoped to `read` and expires, but it is NOT scoped to one
// blob. A client that holds one can read any blob until it expires. Per-blob
// scoping is a GRANT (§77's /grants), which is a later milestone with
// principals behind it — and inventing a lesser version here would be a second
// authorisation model to reconcile with the real one later.

// playbackTokenTTL is how long a playback credential lives.
//
// Long enough to outlast a plausible pause — a player that 401s when someone
// comes back from making tea is a bug people report as "it randomly stops
// working" — and short enough that a leaked one is worthless by the time
// anyone finds it. Two hours covers a film; a longer session re-plans, which
// is one request.
const playbackTokenTTL = 2 * time.Hour

// StartPlaybackRequest is the POST /playback body.
type StartPlaybackRequest struct {
	AssetID  string `json:"asset_id"`
	DeviceID string `json:"device_id"`
	// Verb defaults to watch when the asset has video and listen when it does
	// not, so an ordinary client need not send one.
	Verb string `json:"verb"`
}

// StartPlaybackResponse is what a client plays from.
type StartPlaybackResponse struct {
	SessionID string       `json:"session_id"`
	Plan      PlanResponse `json:"plan"`
	// ContentURL is the ordinary blob endpoint (ADR-0013).
	ContentURL string `json:"content_url"`
	// Token is a short-lived `read` credential for that URL. It is NOT scoped
	// to one blob — see the note at the top of this file.
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	// RenderURL is an absolute, capability-addressed URL that a device with no
	// notion of credentials can simply fetch (ADR-0040).
	//
	// It is what a television, a speaker or a projector is given. Empty when
	// this node cannot mint one — see renderURL — and a client that finds it
	// empty still has ContentURL and Token, which is everything a client that
	// CAN send a header needs.
	RenderURL string `json:"render_url,omitempty"`
	// RenderUnavailable says why RenderURL is empty, when it is empty for a
	// reason worth acting on. A client handed neither a URL nor an
	// explanation cannot tell "your peer does not do this" from "something
	// broke", and will retry the wrong one.
	RenderUnavailable string `json:"render_unavailable,omitempty"`
}

// renderCapabilityTTL is how long a capability URL lives.
//
// Shorter than the playback token, and deliberately so: the token is held by a
// client that can keep a secret in a header, while a capability travels in a
// URL through a television's logs and whatever a renderer chooses to do with
// it. There is no revocation before expiry (ADR-0040), so the expiry is the
// only control there is. Long enough for a film and a pause; short enough that
// a leaked one is worthless by the time anyone finds it in a log.
const renderCapabilityTTL = 6 * time.Hour

// playbackStart is everything one begun playback produced.
//
// It exists so that POST /playback and POST /renderers/{udn}/play share ONE
// implementation of "plan it, refuse it or open a session for it". They differ
// only in what they do with the result — one returns it, the other pushes it
// at a television — and two copies of the session-opening path would be two
// places for "continue watching" to diverge.
type playbackStart struct {
	SessionID         string
	Plan              PlanResponse
	ContentURL        string
	Token             string
	ExpiresAt         time.Time
	RenderURL         string
	RenderUnavailable string
	// Decision is the plan's verdict, carried out for a caller that wants to
	// report it without walking into Plan.
	Decision string
}

// beginPlayback plans, refuses or opens a session.
//
// A refusal comes back as a *problem.Problem rather than an error, because a
// non-DIRECT plan is an ANSWER — the same reasoning the planner gives for
// DecisionUnplayable being a decision rather than an error — and the caller
// should hand it to the client unchanged rather than deciding a status itself.
func (a *API) beginPlayback(ctx context.Context, assetID, deviceID, wantVerb string) (playbackStart, *problem.Problem) {
	device, err := a.deviceProfile(ctx, deviceID)
	if err != nil {
		return playbackStart{}, a.problemFor("device", err)
	}
	media, blobHash, err := a.mediaProfile(ctx, assetID)
	if err != nil {
		return playbackStart{}, a.problemFor("asset", err)
	}
	// Where the bytes come from (§32) before what to do with them (§68).
	route, err := a.routeBlob(ctx, blobHash)
	if err != nil {
		return playbackStart{}, a.problemFor("replica", err)
	}

	plan := playback.Choose(media, device, replicasOf(route))
	rendered := renderPlan(assetID, deviceID, plan, route, blobHash)

	// Everything that is not DIRECT is a refusal, and the refusal is as much
	// the deliverable as the success.
	//
	// M2-10 makes REMUX real and TRANSCODE is beyond this milestone, so those
	// plans cannot be served yet. The client is told WHY — "your device does
	// not declare this codec" — rather than being handed a 501, because a
	// client that cannot distinguish "not supported for you" from "the server
	// is broken" retries the wrong one forever.
	//
	// No session is opened. A session for a playback that cannot happen is
	// state that will never be cleaned up, and it would show up in "continue
	// watching" for something nobody ever watched.
	if !plan.Direct() {
		return playbackStart{}, playbackRefusal(plan, rendered, route)
	}

	verb, err := resolveVerb(wantVerb, media)
	if err != nil {
		return playbackStart{}, problem.BadRequest(err.Error())
	}

	now := a.now().UTC()
	session := playback.Session{
		ID: a.newID(), AssetID: assetID, DeviceID: deviceID,
		Verb: verb, State: playback.StateCreated, CreatedAt: now, UpdatedAt: now,
	}

	expires := now.Add(playbackTokenTTL)
	token, err := a.tokens.Create(ctx,
		"playback "+session.ID, []auth.Scope{auth.ScopeRead}, &expires)
	if err != nil {
		return playbackStart{}, a.problemFor("playback", err)
	}

	var event events.Event
	err = a.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO consumption_sessions
				(id, asset_id, device_id, verb, state, progress_locator, progress_unit,
				 created_at, updated_at, started_at, ended_at)
			VALUES (?, ?, ?, ?, ?, '', '', ?, ?, NULL, NULL)`,
			session.ID, session.AssetID, session.DeviceID,
			string(session.Verb), string(session.State),
			now.Format(timeFormat), now.Format(timeFormat)); err != nil {
			return err
		}
		event, err = a.events.EmitTx(ctx, tx, eventSessionCreated,
			"consumption_session", session.ID,
			map[string]any{
				"session_id": session.ID, "asset_id": session.AssetID,
				"device_id": session.DeviceID, "verb": string(session.Verb),
				"decision": string(plan.Decision),
			})
		return err
	})
	if err != nil {
		if isForeignKeyViolation(err) {
			return playbackStart{}, problem.BadRequest(
				"asset_id and device_id must both name something that exists")
		}
		return playbackStart{}, a.problemFor("playback", err)
	}
	a.events.Publish(event)

	renderURL, unavailable := a.renderURL(ctx, route, blobHash, assetID, now)

	return playbackStart{
		SessionID:         session.ID,
		Plan:              rendered,
		ContentURL:        contentURLFor(route, blobHash),
		Token:             token.Secret,
		ExpiresAt:         expires,
		RenderURL:         renderURL,
		RenderUnavailable: unavailable,
		Decision:          string(plan.Decision),
	}, nil
}

func (a *API) startPlayback(w http.ResponseWriter, r *http.Request) {
	var body StartPlaybackRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	for _, f := range []struct{ name, value string }{
		{"asset_id", body.AssetID}, {"device_id", body.DeviceID},
	} {
		if err := required(f.name, f.value); err != nil {
			httpapi.Fail(w, r, problem.BadRequest(err.Error()))
			return
		}
	}

	started, prob := a.beginPlayback(r.Context(), body.AssetID, body.DeviceID, body.Verb)
	if prob != nil {
		httpapi.Fail(w, r, prob)
		return
	}

	w.Header().Set("Location", httpapi.APIPrefix+"/consumption/sessions/"+started.SessionID)
	a.write(w, r, http.StatusCreated, StartPlaybackResponse{
		SessionID:         started.SessionID,
		Plan:              started.Plan,
		ContentURL:        started.ContentURL,
		Token:             started.Token,
		ExpiresAt:         started.ExpiresAt,
		RenderURL:         started.RenderURL,
		RenderUnavailable: started.RenderUnavailable,
	})
}

// renderURL mints the capability URL a dumb renderer can fetch (ADR-0040).
//
// It returns a URL or a reason, never neither and never both. Every path that
// declines says why, in the same idiom as the rest of §68: a client that
// cannot tell "this peer does not mint these" from "something failed" retries
// the wrong one forever.
func (a *API) renderURL(ctx context.Context, route routing.Decision, blobHash, assetID string, now time.Time) (url, unavailable string) {
	if len(a.renderSecret) == 0 || a.renderBaseURL == "" {
		// Not configured, and not a fault. A node whose clients all send
		// Authorization headers has no use for this route.
		return "", ""
	}
	if blobHash == "" {
		return "", "this asset has no bytes on any peer"
	}
	// A capability is signed by the node that serves the bytes and is valid
	// nowhere else, so this node cannot mint one for a replica it does not
	// hold. Saying so is the deliverable: the operator's next question is
	// "then how do I play it in the living room", and the answer is to
	// replicate it here, which §31 wanted anyway.
	if route.Found && a.selfPeerID != "" && route.Source.PeerID != a.selfPeerID {
		return "", "the replica is on another peer, and a capability is only valid at the peer that signed it"
	}

	// The Asset's declared type, not a guess about the bytes. A renderer
	// refuses application/octet-stream, and the blob endpoint is right to
	// serve nothing else (ADR-0006) — so the type is carried in the
	// capability, fixed here where an Asset is in hand.
	var mime sql.NullString
	if err := a.reader.QueryRowContext(ctx,
		`SELECT mime FROM assets WHERE id = ?`, assetID).Scan(&mime); err != nil {
		a.log.Warn("reading an asset's mime for a render capability",
			"request_id", httpapi.RequestIDFrom(ctx), "asset_id", assetID, "error", err)
		return "", "this asset's media type could not be read"
	}
	// Refused at mint time as well as at serve time. Both are needed and they
	// answer different questions: the serve-side check is the security
	// boundary, and this one is why an operator gets an explanation instead of
	// a television that silently will not play a file.
	if mime.Valid && mime.String != "" && !render.PlayableMIME(mime.String) {
		return "", "this asset is " + mime.String +
			", which is not an audio or video type a renderer can be handed safely"
	}
	if !mime.Valid || mime.String == "" {
		// Refused rather than defaulted. Handing a renderer the wrong type is
		// a device that fails with something unhelpful on screen; handing it
		// none at all is the octet-stream refusal this whole route exists to
		// avoid. Neither is better than an explanation.
		return "", "this asset has no media type recorded, and a renderer will not accept bytes without one"
	}

	token, err := render.Capability{
		BlobHash:  blobHash,
		ExpiresAt: now.Add(renderCapabilityTTL),
		MIME:      mime.String,
	}.Sign(a.renderSecret)
	if err != nil {
		a.log.Error("signing a render capability",
			"request_id", httpapi.RequestIDFrom(ctx), "error", err)
		return "", "this peer could not sign a capability for these bytes"
	}
	// The trailing name is cosmetic and unsigned: some renderers will not
	// issue a GET for a URL whose last segment looks like nothing they
	// recognise. It decides nothing — the type and the bytes both come from
	// the capability.
	return a.renderBaseURL + render.Path(token) + "/" + renderFilename(mime.String), ""
}

// renderFilename is a plausible last path segment for a renderer that sniffs
// one. It is not the asset's filename: that can contain anything, and this
// value is going into a URL that a television will parse with its own rules.
func renderFilename(mime string) string {
	switch {
	case strings.HasPrefix(mime, "video/"):
		return "stream.mp4"
	case strings.HasPrefix(mime, "audio/"):
		return "stream.mp3"
	default:
		return "stream"
	}
}

// playbackRefusal turns a non-DIRECT plan into a problem document.
//
// 409 rather than 501: the request is well-formed and the asset is real; what
// cannot happen is this pairing of asset and device. A 501 says "Heyarr does
// not do this", which is true of TRANSCODE and false of the client's actual
// question.
//
// The detail names the decision and the first reason, which is enough for a
// client to show something useful. The FULL rationale is one request away at
// POST /playback/plan, and that is deliberate: RFC 7807 extension members
// would mean growing the shared Problem type and a custom marshaller so that
// one endpoint could inline a structure another endpoint already returns. The
// plan endpoint exists for exactly this answer.
func playbackRefusal(plan playback.Plan, rendered PlanResponse, route routing.Decision) *problem.Problem {
	if plan.Decision == playback.DecisionUnplayable {
		// The routing refusal is inlined rather than pointed at, which is the
		// one place this endpoint departs from "the plan endpoint has the full
		// rationale" — deliberately. A codec refusal names a fact about the
		// client's own device and the client can act on the summary; a routing
		// refusal names facts about MACHINES, and the person who needs them is
		// an operator reading a support ticket that quotes one error string.
		// "Unavailable" and nothing else is the outage that takes three hours.
		return problem.Conflict(route.Refusal() +
			"; POST /api/v1/playback/plan for the full rationale")
	}
	detail := "this asset needs " + string(plan.Decision) +
		" on this device, which Milestone 2 cannot serve"
	if len(rendered.Reasons) > 0 {
		detail += ": " + rendered.Reasons[0].Detail
	}
	return problem.Conflict(detail + "; POST /api/v1/playback/plan for the full rationale")
}

// resolveVerb picks watching or listening when the client did not say.
//
// It is derived from the probe rather than the extension, because the probe is
// what actually knows: a .mkv holding only audio is a legitimate thing, and
// telling a client it is "watching" it would put it in the wrong row of every
// continue-watching list.
func resolveVerb(requested string, media playback.MediaProfile) (playback.Verb, error) {
	if requested != "" {
		return playback.ParseVerb(requested)
	}
	if media.Known && media.VideoCodec == "" {
		return playback.VerbListen, nil
	}
	return playback.VerbWatch, nil
}
