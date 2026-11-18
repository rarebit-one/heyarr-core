package resources

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
	"github.com/rarebit-one/heyarr-core/internal/domain/routing"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
)

// The playback planner's API surface (§68).
//
// POST /api/v1/playback/plan answers "what should this device do with this
// asset", without opening a session. Planning and consuming are separate
// deliberately: a client shows a "play" button by planning, and opens a session
// when someone presses it. Fusing them would mean every hover created state.

// PlanRequest is the POST /playback/plan body.
type PlanRequest struct {
	// AssetID names the file to plan. It always wins: when it is given,
	// WorkID and EditionID are ignored and the plan is exactly what it was
	// before a work could be named.
	AssetID string `json:"asset_id"`
	// WorkID names a film instead, and the node chooses which of its copies
	// to plan from what the client can show (#885). EditionID narrows the
	// choice to one edition's copies.
	WorkID    string `json:"work_id,omitempty"`
	EditionID string `json:"edition_id,omitempty"`
	// DeviceID names a registered device. Required unless Client is given.
	DeviceID string `json:"device_id"`
	// Client is what the CALLER can decode, for a client that is not a
	// registered device — a phone, a browser (ADR-0069). When present the
	// answer carries the leg (mode, url, mime, reason, source) as well as the
	// decision, and device_id may be omitted.
	Client *ClientCaps `json:"client,omitempty"`
	// Preference is the person's playback quality (#887): their synced
	// default with this device's override laid over it. It is personal state,
	// which the node cannot read, so the client resolves it and sends the
	// result here. Absent is "fit": the plan is exactly what it was before.
	Preference *PlanPreference `json:"preference,omitempty"`
}

// PlanPreference is a playback quality preference on the wire (#887).
type PlanPreference struct {
	// Quality is fit, best or data_saver. Empty is fit.
	Quality string `json:"quality,omitempty"`
	// MaxHeight and MaxBitrateBPS are the person's own cap, a further
	// ceiling over what the screen can show. Zero is no cap.
	MaxHeight     int64 `json:"max_height,omitempty"`
	MaxBitrateBPS int64 `json:"max_bitrate_bps,omitempty"`
}

// preference is the request's preference in the domain's shape, or an error
// naming the field a client got wrong.
func (body PlanRequest) preference() (playback.Preference, error) {
	p := body.Preference
	if p == nil {
		return playback.Preference{}, nil
	}
	q := playback.Quality(p.Quality)
	switch {
	case !q.Valid():
		return playback.Preference{}, errors.New("preference.quality must be fit, best or data_saver")
	case p.MaxHeight < 0 || p.MaxBitrateBPS < 0:
		return playback.Preference{}, errors.New("preference.max_height and preference.max_bitrate_bps must not be negative")
	}
	if q == "" {
		q = playback.QualityFit
	}
	return playback.Preference{Quality: q, MaxHeight: p.MaxHeight, MaxBitrateBPS: p.MaxBitrateBPS}, nil
}

// renderPreference echoes the preference the plan applied, so a client can
// tell a node that honoured it from one that predates #887 and ignored the
// field. Nil when the request carried none.
func renderPreference(body PlanRequest, p playback.Preference) *PlanPreference {
	if body.Preference == nil {
		return nil
	}
	return &PlanPreference{Quality: string(p.Quality), MaxHeight: p.MaxHeight, MaxBitrateBPS: p.MaxBitrateBPS}
}

// PlanResponse is the planner's answer, rendered.
type PlanResponse struct {
	AssetID  string `json:"asset_id"`
	DeviceID string `json:"device_id"`
	Decision string `json:"decision"`
	// Reasons is why the decision is not DIRECT, or why nothing can be played.
	// "Why is my television transcoding this" is the question this field
	// exists to answer.
	Reasons []PlanReason `json:"reasons"`
	PeerID  string       `json:"peer_id,omitempty"`
	Remote  bool         `json:"remote"`
	// ContentURL is where the bytes are, for a DIRECT plan. Absent otherwise —
	// a REMUX or TRANSCODE plan has no bytes to point at yet, and offering the
	// original would be inviting the client to play what the plan just said it
	// cannot.
	ContentURL string `json:"content_url,omitempty"`
	// Routing is why this peer, and why not the others (§32, M4-14).
	//
	// Present on every plan, including a refusal — especially on a refusal.
	// "Unavailable" with nothing after it is the outage that takes three hours
	// to diagnose, because it cannot tell a peer that is down from a peer that
	// never had the bytes, and those have different fixes.
	Routing *RoutingResponse `json:"routing,omitempty"`

	// The leg, present only when the request carried `client` (ADR-0069).
	//
	// Mode is direct, stream or unplayable; URL is what to fetch — the blob
	// endpoint or the stream route — and MIME is what it will be served as.
	// Reason is one sentence saying why the mode is not a clean direct.
	Mode   string `json:"mode,omitempty"`
	URL    string `json:"url,omitempty"`
	MIME   string `json:"mime,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Source is what the probe found, so a client can show "AC-3 5.1" next
	// to the reason it is being served a stream.
	Source *SourceInfo `json:"source,omitempty"`

	// Selection is present when the request named a work rather than an
	// asset (#885): the copy chosen, why, and every other copy with why not.
	// AssetID above is the chosen copy.
	Selection *SelectionResponse `json:"selection,omitempty"`
	// Preference is the quality preference the plan applied (#887), present
	// when the request carried one. Its absence on an answer to a request
	// that sent one means the node ignored it.
	Preference *PlanPreference `json:"preference,omitempty"`
}

// SelectionResponse is the copy selection, rendered (#885).
type SelectionResponse struct {
	Chosen SelectionCandidate `json:"chosen"`
	// Alternatives is every other copy, best first, each with the reason it
	// lost. Never nil, like Routing.Rejected.
	Alternatives []SelectionCandidate `json:"alternatives"`
}

// SelectionCandidate is one copy of the work as selection saw it: enough for
// a client to offer the alternatives as a manual choice.
type SelectionCandidate struct {
	AssetID      string `json:"asset_id"`
	EditionID    string `json:"edition_id"`
	EditionLabel string `json:"edition_label"`
	PictureFacts
	VideoCodec string     `json:"video_codec,omitempty"`
	Reason     PlanReason `json:"reason"`
}

// RoutingResponse is the source selection, rendered (§32).
type RoutingResponse struct {
	// PeerID is the selected source. Empty when routing refused.
	PeerID string `json:"peer_id,omitempty"`
	// Reason is why that peer won: site_local, or cross_site_fallback with the
	// fallback recorded as one (§31).
	Reason *PlanReason `json:"reason,omitempty"`
	// Rejected is every peer considered and not selected, each with every
	// reason it was not.
	Rejected []RoutingRejection `json:"rejected"`
}

// RoutingRejection is one peer that was considered and passed over.
type RoutingRejection struct {
	PeerID  string       `json:"peer_id"`
	Name    string       `json:"name"`
	Site    string       `json:"site"`
	Reasons []PlanReason `json:"reasons"`
}

// PlanReason is one contribution to a decision.
type PlanReason struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func (a *API) planPlayback(w http.ResponseWriter, r *http.Request) {
	var body PlanRequest
	if !decodeOr400(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.AssetID) == "" && strings.TrimSpace(body.WorkID) == "" {
		httpapi.Fail(w, r, problem.BadRequest("asset_id or work_id is required"))
		return
	}
	if body.Client == nil {
		if err := required("device_id", body.DeviceID); err != nil {
			httpapi.Fail(w, r, problem.BadRequest(err.Error()))
			return
		}
	}
	pref, err := body.preference()
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	// An explicit asset wins; only a request naming a work alone is chosen
	// for (#885).
	var selection *SelectionResponse
	if strings.TrimSpace(body.AssetID) == "" {
		var ok bool
		if selection, ok = a.selectCopy(w, r, body, pref); !ok {
			return
		}
		body.AssetID = selection.Chosen.AssetID
	}
	// A client that says what it can decode gets the leg as well as the
	// decision, and need not be a registered device (ADR-0069).
	if body.Client != nil {
		a.planForClient(w, r, body, pref, selection)
		return
	}

	device, err := a.deviceProfile(r.Context(), body.DeviceID)
	if err != nil {
		a.fail(w, r, "device", err)
		return
	}
	device.MaxHeight = pref.Height(device.MaxHeight)
	media, blobHash, err := a.mediaProfile(r.Context(), body.AssetID, nil)
	if err != nil {
		a.fail(w, r, "asset", err)
		return
	}
	route, err := a.routeBlob(r.Context(), blobHash)
	if err != nil {
		a.fail(w, r, "replica", err)
		return
	}

	plan := playback.Choose(media, device, replicasOf(route))
	out := renderPlan(body.AssetID, body.DeviceID, plan, route, blobHash)
	out.Selection = selection
	out.Preference = renderPreference(body, pref)
	a.write(w, r, http.StatusOK, out)
}

// selectCopy chooses which of a film's copies to plan (#885), or fails the
// request and reports false.
//
// What the client can show comes from its declaration; anything it leaves
// unstated is filled from the registered device it names, field by field, so
// a television that registered once need declare nothing per request. The
// person's preference is laid over the result (#887).
func (a *API) selectCopy(
	w http.ResponseWriter, r *http.Request, body PlanRequest, pref playback.Preference,
) (*SelectionResponse, bool) {
	ctx := r.Context()
	var contentType string
	if err := a.reader.QueryRowContext(ctx,
		`SELECT content_type FROM works WHERE id = ?`, body.WorkID).Scan(&contentType); err != nil {
		a.fail(w, r, "work", err)
		return nil, false
	}
	if contentType != "movie" {
		// A season edition holds every episode, so "a copy of this work"
		// would offer a different episode. Copies of an episode are planned
		// by asset until a client can name the item.
		httpapi.Fail(w, r, problem.BadRequest(
			"work_id chooses among a film's copies; plan an episode or track by asset_id"))
		return nil, false
	}

	fit, err := a.selectionFit(ctx, body)
	if err != nil {
		a.fail(w, r, "device", err)
		return nil, false
	}
	fit = pref.Apply(fit)
	copies, err := a.queryCopies(ctx, copiesOfFilm, body.WorkID, body.EditionID, body.EditionID)
	if err != nil {
		a.fail(w, r, "asset", err)
		return nil, false
	}
	byID := make(map[string]workCopy, len(copies))
	candidates := make([]playback.Copy, 0, len(copies))
	for _, c := range copies {
		if c.Probed && !c.Picture {
			continue // probed, and no picture: not a copy of the film
		}
		byID[c.AssetID] = c
		candidates = append(candidates, c.copy())
	}
	sel, ok := playback.Select(candidates, fit)
	if !ok {
		httpapi.Fail(w, r, problem.NotFound("that work holds no playable copy"))
		return nil, false
	}

	render := func(c playback.Candidate) SelectionCandidate {
		row := byID[c.AssetID]
		out := SelectionCandidate{
			AssetID: row.AssetID, EditionID: row.EditionID, EditionLabel: row.EditionLabel,
			PictureFacts: row.PictureFacts,
			Reason:       PlanReason{Code: c.Reason.Code, Detail: c.Reason.Detail},
		}
		if row.Picture {
			out.VideoCodec = row.Video.Codec
		}
		return out
	}
	out := &SelectionResponse{
		Chosen:       render(sel.Chosen),
		Alternatives: make([]SelectionCandidate, 0, len(sel.Passed)),
	}
	for _, p := range sel.Passed {
		out.Alternatives = append(out.Alternatives, render(p))
	}
	return out, true
}

// selectionFit is what the client can show: its declaration, with each field
// it leaves unstated filled from the registered device it names.
func (a *API) selectionFit(ctx context.Context, body PlanRequest) (playback.Fit, error) {
	var (
		fit       playback.Fit
		hdrStated bool
	)
	if c := body.Client; c != nil {
		fit.MaxHeight, fit.MaxBitrateBPS, fit.VideoCodecs = int64(c.MaxHeight), c.MaxBitrateBPS, c.Video
		if c.SupportsHDR != nil {
			fit.SupportsHDR, hdrStated = *c.SupportsHDR, true
		}
	}
	if body.DeviceID == "" {
		return fit, nil
	}
	device, err := a.deviceProfile(ctx, body.DeviceID)
	if err != nil {
		return playback.Fit{}, err
	}
	if fit.MaxHeight == 0 {
		fit.MaxHeight = device.MaxHeight
	}
	if fit.MaxBitrateBPS == 0 {
		fit.MaxBitrateBPS = device.MaxBitrateBPS
	}
	if !hdrStated {
		fit.SupportsHDR = device.SupportsHDR
	}
	if len(fit.VideoCodecs) == 0 {
		fit.VideoCodecs = device.VideoCodecs
	}
	return fit, nil
}

// renderPlan maps the domain's plan onto the wire type. It is shared with
// POST /playback so the two endpoints cannot drift into describing one
// decision two ways.
func renderPlan(assetID, deviceID string, plan playback.Plan, route routing.Decision, blobHash string) PlanResponse {
	out := PlanResponse{
		AssetID: assetID, DeviceID: deviceID,
		Decision: string(plan.Decision), PeerID: plan.PeerID, Remote: plan.Remote,
		Reasons: make([]PlanReason, 0, len(plan.Reasons)),
		Routing: renderRouting(route),
	}
	for _, reason := range plan.Reasons {
		out.Reasons = append(out.Reasons, PlanReason{Code: reason.Code, Detail: reason.Detail})
	}
	if plan.Direct() {
		out.ContentURL = contentURLFor(route, blobHash)
	}
	return out
}

// renderRouting maps the routing decision onto the wire.
//
// Rejected is never nil, so a client that iterates it does not have to
// distinguish "no peers were rejected" from "the field is missing" — the same
// reason Reasons is made rather than declared.
func renderRouting(route routing.Decision) *RoutingResponse {
	out := &RoutingResponse{Rejected: make([]RoutingRejection, 0, len(route.Rejected))}
	if route.Found {
		out.PeerID = route.Source.PeerID
		out.Reason = &PlanReason{Code: route.Reason.Code, Detail: route.Reason.Detail}
	}
	for _, r := range route.Rejected {
		rejection := RoutingRejection{
			PeerID: r.PeerID, Name: r.Name, Site: r.Site,
			Reasons: make([]PlanReason, 0, len(r.Reasons)),
		}
		for _, reason := range r.Reasons {
			rejection.Reasons = append(rejection.Reasons,
				PlanReason{Code: reason.Code, Detail: reason.Detail})
		}
		out.Rejected = append(out.Rejected, rejection)
	}
	return out
}

// deviceProfile loads what a device declared (M2-05).
func (a *API) deviceProfile(ctx context.Context, id string) (playback.DeviceProfile, error) {
	var (
		p                        playback.DeviceProfile
		containers, video, audio string
		hdr                      int
	)
	if err := a.reader.QueryRowContext(ctx, `
		SELECT containers, video_codecs, audio_codecs,
		       max_width, max_height, max_bitrate_bps, supports_hdr
		FROM devices WHERE id = ?`, id).
		Scan(&containers, &video, &audio,
			&p.MaxWidth, &p.MaxHeight, &p.MaxBitrateBPS, &hdr); err != nil {
		return playback.DeviceProfile{}, err
	}
	p.SupportsHDR = hdr == 1
	for _, pair := range []struct {
		raw  string
		dest *[]string
	}{{containers, &p.Containers}, {video, &p.VideoCodecs}, {audio, &p.AudioCodecs}} {
		list, err := decodeStringList(pair.raw)
		if err != nil {
			return playback.DeviceProfile{}, err
		}
		*pair.dest = list
	}
	return p, nil
}

// mediaProfile loads what the probe found, if anything.
//
// Known stays false when there is no probe row, and that is the interesting
// case rather than an edge one: it is the state of every blob on a node with
// no ffprobe (ADR-0023), and the planner's answer for it is deliberate rather
// than incidental.
func (a *API) mediaProfile(ctx context.Context, assetID string, audioLanguages []string) (playback.MediaProfile, string, error) {
	var blobHash sql.NullString
	if err := a.reader.QueryRowContext(ctx,
		`SELECT blob_hash FROM assets WHERE id = ?`, assetID).Scan(&blobHash); err != nil {
		return playback.MediaProfile{}, "", err
	}
	// A linked asset has no blob at all (ADR-0020) and therefore no probe.
	// This is the fourth place that bites; see migration 00008.
	if !blobHash.Valid {
		return playback.MediaProfile{}, "", nil
	}

	var (
		container string
		duration  sql.NullFloat64
		bitrate   sql.NullInt64
		streams   string
	)
	err := a.reader.QueryRowContext(ctx, `
		SELECT container, duration_seconds, bitrate_bps, streams
		FROM blob_probes WHERE blob_hash = ?`, blobHash.String).
		Scan(&container, &duration, &bitrate, &streams)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Not an error. Nothing has probed these bytes, which the planner
		// handles explicitly.
		return playback.MediaProfile{}, blobHash.String, nil
	case err != nil:
		return playback.MediaProfile{}, blobHash.String, err
	}

	var parsed []probe.Stream
	if err := json.Unmarshal([]byte(streams), &parsed); err != nil {
		return playback.MediaProfile{}, blobHash.String, err
	}
	result := probe.Result{Container: container, Streams: parsed}
	if bitrate.Valid {
		result.BitrateBPS = bitrate.Int64
	}
	if duration.Valid {
		result.DurationSec = duration.Float64
	}
	media := profileFromProbe(result, audioLanguages)
	return media, blobHash.String, nil
}

// replicasFor lists the peers holding a blob, marking which are local.
//
// "Local" is same-site, per §31. It answers only "do usable bytes exist", which
// is the question a LOCAL job asks: POST /playback/remux enqueues work against
// this node's own CAS and does not route anywhere, so it needs existence and
// not a source. Read routing is routeBlob, which applies §32's health and
// locality preference and reports why every other peer lost.
func (a *API) replicasFor(ctx context.Context, blobHash string) ([]playback.Replica, error) {
	if blobHash == "" {
		return nil, nil
	}
	rows, err := a.reader.QueryContext(ctx, `
		SELECT r.peer_id, p.site, (SELECT site FROM peers WHERE is_self = 1)
		FROM replicas r JOIN peers p ON p.id = r.peer_id
		WHERE r.blob_hash = ? AND r.state = 'present'
		ORDER BY p.is_self DESC, r.peer_id ASC`, blobHash)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []playback.Replica
	for rows.Next() {
		var peerID, site string
		var selfSite sql.NullString
		if err := rows.Scan(&peerID, &site, &selfSite); err != nil {
			return nil, err
		}
		out = append(out, playback.Replica{PeerID: peerID, Local: site == selfSite.String})
	}
	return out, rows.Err()
}
