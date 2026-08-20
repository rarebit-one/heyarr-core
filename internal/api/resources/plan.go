package resources

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
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
	AssetID  string `json:"asset_id"`
	DeviceID string `json:"device_id"`
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
	replicas, err := a.replicasFor(r, blobHash)
	if err != nil {
		a.fail(w, r, "replica", err)
		return
	}

	plan := playback.Choose(media, device, replicas)
	a.write(w, r, http.StatusOK, renderPlan(body.AssetID, body.DeviceID, plan, blobHash))
}

// renderPlan maps the domain's plan onto the wire type. It is shared with
// POST /playback so the two endpoints cannot drift into describing one
// decision two ways.
func renderPlan(assetID, deviceID string, plan playback.Plan, blobHash string) PlanResponse {
	out := PlanResponse{
		AssetID: assetID, DeviceID: deviceID,
		Decision: string(plan.Decision), PeerID: plan.PeerID, Remote: plan.Remote,
		Reasons: make([]PlanReason, 0, len(plan.Reasons)),
	}
	for _, reason := range plan.Reasons {
		out.Reasons = append(out.Reasons, PlanReason{Code: reason.Code, Detail: reason.Detail})
	}
	if plan.Direct() && blobHash != "" {
		out.ContentURL = probe.BlobURL("", blobHash)
	}
	return out
}

// deviceProfile loads what a device declared (M2-05).
func (a *API) deviceProfile(r *http.Request, id string) (playback.DeviceProfile, error) {
	var (
		p                        playback.DeviceProfile
		containers, video, audio string
		hdr                      int
	)
	if err := a.reader.QueryRowContext(r.Context(), `
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
func (a *API) mediaProfile(r *http.Request, assetID string) (playback.MediaProfile, string, error) {
	var blobHash sql.NullString
	if err := a.reader.QueryRowContext(r.Context(),
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
	err := a.reader.QueryRowContext(r.Context(), `
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
// "Local" is same-site, per §31. With exactly one peer (ADR-0010) that is
// always this node, so the remote branch is expressible and untested against
// reality until Milestone 4 — which is stated on playback.Plan.Remote too,
// because a caveat only in the domain is a caveat the API layer will forget.
func (a *API) replicasFor(r *http.Request, blobHash string) ([]playback.Replica, error) {
	if blobHash == "" {
		return nil, nil
	}
	rows, err := a.reader.QueryContext(r.Context(), `
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
