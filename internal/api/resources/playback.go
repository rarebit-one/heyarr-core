package resources

import (
	"database/sql"
	"net/http"
	"time"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
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

	device, err := a.deviceProfile(r, body.DeviceID)
	if err != nil {
		a.fail(w, r, "device", err)
		return
	}
	media, blobHash, err := a.mediaProfile(r, body.AssetID)
	if err != nil {
		a.fail(w, r, "asset", err)
		return
	}
	// Where the bytes come from (§32) before what to do with them (§68).
	route, err := a.routeBlob(r, blobHash)
	if err != nil {
		a.fail(w, r, "replica", err)
		return
	}

	plan := playback.Choose(media, device, replicasOf(route))
	rendered := renderPlan(body.AssetID, body.DeviceID, plan, route, blobHash)

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
		httpapi.Fail(w, r, playbackRefusal(plan, rendered, route))
		return
	}

	verb, err := resolveVerb(body.Verb, media)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	now := a.now().UTC()
	session := playback.Session{
		ID: a.newID(), AssetID: body.AssetID, DeviceID: body.DeviceID,
		Verb: verb, State: playback.StateCreated, CreatedAt: now, UpdatedAt: now,
	}

	expires := now.Add(playbackTokenTTL)
	token, err := a.tokens.Create(r.Context(),
		"playback "+session.ID, []auth.Scope{auth.ScopeRead}, &expires)
	if err != nil {
		a.fail(w, r, "playback", err)
		return
	}

	var event events.Event
	err = a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO consumption_sessions
				(id, asset_id, device_id, verb, state, progress_locator, progress_unit,
				 created_at, updated_at, started_at, ended_at)
			VALUES (?, ?, ?, ?, ?, '', '', ?, ?, NULL, NULL)`,
			session.ID, session.AssetID, session.DeviceID,
			string(session.Verb), string(session.State),
			now.Format(timeFormat), now.Format(timeFormat)); err != nil {
			return err
		}
		event, err = a.events.EmitTx(r.Context(), tx, eventSessionCreated,
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
			httpapi.Fail(w, r, problem.BadRequest(
				"asset_id and device_id must both name something that exists"))
			return
		}
		a.fail(w, r, "playback", err)
		return
	}
	a.events.Publish(event)

	w.Header().Set("Location", httpapi.APIPrefix+"/consumption/sessions/"+session.ID)
	a.write(w, r, http.StatusCreated, StartPlaybackResponse{
		SessionID:  session.ID,
		Plan:       rendered,
		ContentURL: contentURLFor(route, blobHash),
		Token:      token.Secret,
		ExpiresAt:  expires,
	})
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
