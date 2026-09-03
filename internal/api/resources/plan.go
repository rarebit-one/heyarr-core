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
)

// The playback planner's API surface (§68).
//
// POST /api/v1/playback/plan answers "what should this device do with this
// asset", without opening a session. Planning and consuming are separate
// deliberately: a client shows a "play" button by planning, and opens a session
// when someone presses it. Fusing them would mean every hover created state.

// PlanRequest is the POST /playback/plan body.
type PlanRequest struct {
	AssetID string `json:"asset_id"`
	// DeviceID names a registered device. Required unless Client is given.
	DeviceID string `json:"device_id"`
	// Client is what the CALLER can decode, for a client that is not a
	// registered device — a phone, a browser (ADR-0069). When present the
	// answer carries the leg (mode, url, mime, reason, source) as well as the
	// decision, and device_id may be omitted.
	Client *ClientCaps `json:"client,omitempty"`
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
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("asset_id", body.AssetID); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	// A client that says what it can decode gets the leg as well as the
	// decision, and need not be a registered device (ADR-0069).
	if body.Client != nil {
		a.planForClient(w, r, body)
		return
	}
	if err := required("device_id", body.DeviceID); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	device, err := a.deviceProfile(r.Context(), body.DeviceID)
	if err != nil {
		a.fail(w, r, "device", err)
		return
	}
	media, blobHash, err := a.mediaProfile(r.Context(), body.AssetID)
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
	a.write(w, r, http.StatusOK, renderPlan(body.AssetID, body.DeviceID, plan, route, blobHash))
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
func (a *API) mediaProfile(ctx context.Context, assetID string) (playback.MediaProfile, string, error) {
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

	var parsed []ProbeStream
	if err := json.Unmarshal([]byte(streams), &parsed); err != nil {
		return playback.MediaProfile{}, blobHash.String, err
	}
	media := playback.MediaProfile{Known: true, Container: container}
	if bitrate.Valid {
		media.BitrateBPS = bitrate.Int64
	}
	for _, s := range parsed {
		switch s.Type {
		case "video":
			if media.VideoCodec == "" {
				media.VideoCodec = s.Codec
				media.Width, media.Height = s.Width, s.Height
				// HDR detection from a stream profile is Milestone 3's
				// identification work. Reporting false here is a claim the
				// planner then acts on, so it is stated rather than implied:
				// an HDR file on a non-HDR device will currently plan DIRECT
				// and look wrong on the television, which is a visible,
				// recoverable failure rather than a silent one.
				media.HDR = strings.Contains(strings.ToLower(s.Profile), "hdr")
			}
		case "audio":
			if media.AudioCodec == "" {
				media.AudioCodec, media.Channels = s.Codec, s.Channels
			}
		}
	}
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
