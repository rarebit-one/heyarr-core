package resources

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
)

// The device-aware streaming leg (§33, §68, ADR-0069).
//
// A client that says what it can decode is told, per asset, whether to fetch
// the blob (`direct`) or a repackaged stream (`stream`) — and the stream is a
// second byte route, GET /api/v1/playback/stream/{token}, that runs one ffmpeg
// per client and writes fragmented MP4 as it is produced.
//
// # Why this is not the departure ADR-0013 forbids
//
// ADR-0013 keeps the blob endpoint a byte contract with four consumers and
// says a player-shaped session token must not be added TO IT. Nothing here
// touches it: `direct` is still that endpoint, unchanged, and `stream` is a
// different route serving different bytes — bytes that do not exist until
// ffmpeg makes them and have no digest, no length and no ranges. They could
// not honour the blob contract, so they do not pretend to.
//
// # What the controller does and does not do here
//
// §32 keeps the controller out of the content data path, and for `direct` it
// still is. For `stream` the node that HOLDS the bytes produces the stream —
// a token is minted only when the routed replica is this node — so a
// controller with no local replica hands the client the direct URL on the
// other peer and says why. Under `heyarr all` that is one machine either way.

// ClientCaps is what a client declares it can decode, on POST /playback/plan.
//
// Codec names are ffprobe's (`h264`, `hevc`, `ac3`, `eac3`, `aac`, `mp2`),
// container names are the ordinary ones (`mp4`, `mkv`, `webm`, `avi`).
type ClientCaps struct {
	Containers []string `json:"containers"`
	Video      []string `json:"video"`
	Audio      []string `json:"audio"`
	// MaxHeight is the tallest picture the client will take. Zero means no
	// limit.
	MaxHeight int `json:"max_height"`
}

func (c ClientCaps) profile() playback.ClientProfile {
	return playback.ClientProfile{
		Containers: c.Containers, VideoCodecs: c.Video, AudioCodecs: c.Audio, MaxHeight: c.MaxHeight,
	}
}

// SourceInfo is what the probe found, rendered for a client deciding whether
// to trust the plan.
type SourceInfo struct {
	Container string `json:"container"`
	Video     string `json:"video,omitempty"`
	Audio     string `json:"audio,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

// Mode values on a plan answered for a client.
const (
	ModeDirect     = "direct"
	ModeStream     = "stream"
	ModeUnplayable = "unplayable"
)

// streamMIME is what the stream route serves. Always: the repackage has one
// output shape (ADR-0069).
const streamMIME = "video/mp4"

// streamPath is the stream route, relative like content_url.
const streamPath = httpapi.APIPrefix + "/playback/stream/"

// BlobLocator says where a blob's bytes are on this node's filesystem, for
// ffmpeg and ffprobe, which need a file.
type BlobLocator interface {
	SourcePath(ctx context.Context, blobHash string) (string, error)
}

// PathProber describes a local file. It is *probe.Prober's ProbePath, as an
// interface so the harness can run without ffprobe.
type PathProber interface {
	ProbePath(ctx context.Context, path string) (probe.Result, probe.Stats, error)
}

// PlaybackStreamer produces the repackaged stream. It is *ffmpeg.Streamer, as
// an interface so the harness can prove the route without ffmpeg.
type PlaybackStreamer interface {
	Stream(ctx context.Context, spec ffmpeg.StreamSpec, w io.Writer) error
	Active() int
}

// planForClient answers POST /playback/plan when the body carries `client`.
//
// It reuses the planner (Choose) for the decision and its reasons, rendering
// the client's declaration as a device profile when no device_id was sent, so
// a client and a registered device are explained in one vocabulary — and it
// adds the leg: what to fetch.
func (a *API) planForClient(w http.ResponseWriter, r *http.Request, body PlanRequest) {
	ctx := r.Context()
	client := body.Client.profile()

	media, blobHash, err := a.mediaProfile(ctx, body.AssetID)
	if err != nil {
		a.fail(w, r, "asset", err)
		return
	}
	// Nothing has probed these bytes and this node can: probe now, so the
	// answer is a finding rather than a guess, and cache it where the worker
	// would have (blob_probes, keyed by hash — invariant 1).
	if !media.Known && blobHash != "" {
		if probed, ok := a.probeOnDemand(ctx, blobHash); ok {
			media = probed
		}
	}

	device := client.DeviceProfile()
	deviceID := body.DeviceID
	if deviceID != "" {
		if device, err = a.deviceProfile(ctx, deviceID); err != nil {
			a.fail(w, r, "device", err)
			return
		}
	}
	route, err := a.routeBlob(ctx, blobHash)
	if err != nil {
		a.fail(w, r, "replica", err)
		return
	}

	plan := playback.Choose(media, device, replicasOf(route))
	out := renderPlan(body.AssetID, deviceID, plan, route, blobHash)
	if media.Known {
		out.Source = &SourceInfo{
			Container: media.Container, Video: media.VideoCodec, Audio: media.AudioCodec,
			Width: media.Width, Height: media.Height,
		}
	}

	if plan.Decision == playback.DecisionUnplayable {
		out.Mode = ModeUnplayable
		out.Reason = route.Refusal()
		a.write(w, r, http.StatusOK, out)
		return
	}

	leg := playback.Negotiate(media, client)
	direct := func(reason string) {
		out.Mode, out.URL, out.Reason = ModeDirect, contentURLFor(route, blobHash), reason
		out.MIME = a.assetMIME(ctx, body.AssetID)
	}
	switch {
	case leg.Direct():
		direct(leg.Reason)
	case a.streamer == nil:
		// The client cannot decode it and this node cannot repackage it. The
		// honest answer is the bytes and the reason — a client that knows
		// why can say "this needs ffmpeg on the server" instead of showing a
		// black player (ADR-0023's stance, one route over).
		direct(leg.Reason + "; this node has no ffmpeg, so the bytes are handed over as they are")
	case !route.Source.IsSelf:
		// A stream is produced where the bytes are, and this node does not
		// hold them. Same idiom as the render capability (ADR-0040).
		direct(leg.Reason + "; the replica is on another peer, and a stream is produced only by the peer holding the bytes")
	default:
		id, _ := httpapi.IdentityFrom(ctx)
		token, err := streamToken{
			BlobHash: blobHash, Subject: streamSubject(id),
			CopyVideo: leg.CopyVideo, CopyAudio: leg.CopyAudio, MaxHeight: leg.TargetHeight,
			ExpiresAt: a.now().UTC().Add(streamTokenTTL),
		}.sign(a.streamKey)
		if err != nil {
			a.log.Error("signing a stream token", "request_id", httpapi.RequestIDFrom(ctx), "error", err)
			httpapi.Fail(w, r, problem.Internal())
			return
		}
		out.Mode, out.URL, out.MIME, out.Reason = ModeStream, streamPath+token, streamMIME, leg.Reason
	}
	a.write(w, r, http.StatusOK, out)
}

// probeOnDemand probes a blob this node holds, records the result, and
// reports whether it could. Every failure is a log line and "no": a plan for
// media that cannot be probed is a direct plan with the guess declared, which
// is the planner's existing answer to an unmeasured file.
func (a *API) probeOnDemand(ctx context.Context, blobHash string) (playback.MediaProfile, bool) {
	if a.prober == nil || a.blobs == nil {
		return playback.MediaProfile{}, false
	}
	path, err := a.blobs.SourcePath(ctx, blobHash)
	if err != nil {
		// Not held here (a remote replica) or not a whole blob: nothing to
		// probe, and not a fault.
		return playback.MediaProfile{}, false
	}
	result, stats, err := a.prober.ProbePath(ctx, path)
	if err != nil {
		a.log.Warn("probing a blob for a playback plan", "request_id", httpapi.RequestIDFrom(ctx),
			"blob", blobHash, "error", err)
		return playback.MediaProfile{}, false
	}
	if a.catalog != nil {
		if err := a.catalog.RecordProbe(ctx, blobHash, result, stats, a.now().UTC()); err != nil {
			// The answer is still good; only the cache write failed. Say so
			// and answer anyway rather than making the client wait on a
			// worker that may not exist.
			a.log.Warn("caching an on-demand probe", "request_id", httpapi.RequestIDFrom(ctx),
				"blob", blobHash, "error", err)
		}
	}
	return profileFromProbe(result), true
}

// profileFromProbe is mediaProfile's mapping, over a live result rather than
// a stored row. The two must agree, which is why both take the FIRST stream of
// each type and read HDR off the profile name the same way.
func profileFromProbe(result probe.Result) playback.MediaProfile {
	media := playback.MediaProfile{Known: true, Container: result.Container, BitrateBPS: result.BitrateBPS}
	if v, ok := result.VideoStream(); ok {
		media.VideoCodec, media.Width, media.Height = v.Codec, v.Width, v.Height
		media.HDR = strings.Contains(strings.ToLower(v.Profile), "hdr")
	}
	if au, ok := result.AudioStream(); ok {
		media.AudioCodec, media.Channels = au.Codec, au.Channels
	}
	return media
}

// assetMIME is the asset's declared type, or empty when it has none.
func (a *API) assetMIME(ctx context.Context, assetID string) string {
	var mime sql.NullString
	if err := a.reader.QueryRowContext(ctx, `SELECT mime FROM assets WHERE id = ?`, assetID).Scan(&mime); err != nil {
		return ""
	}
	return mime.String
}

// streamPlayback serves GET /api/v1/playback/stream/{token}.
//
// # Every refusal is the same 404
//
// A token that does not verify, has expired, was minted for another
// credential, or names a blob this node no longer holds is answered
// identically, with the reason going to the log under a bounded label. The
// route is behind the authentication middleware like every other, so the
// caller is already known; what it must not learn from the status is whether
// the token it is probing with is a real one. This is the render route's
// stance (ADR-0040) applied to a route that DOES know who is asking.
//
// # No ranges, no length, no seeking
//
// The bytes do not exist until ffmpeg makes them, so there is no length to
// declare and no offset to serve from. A player therefore treats this as a
// live, unseekable stream (Media3 does, given fragmented MP4 with no Range
// support). `?start=<seconds>` restarts the stream from an input seek — a new
// request, a new ffmpeg — which is what a client does for a seek in v1
// (ADR-0069).
func (a *API) streamPlayback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := httpapi.IdentityFrom(ctx)
	tok, err := verifyStreamToken(a.streamKey, chi.URLParam(r, "token"), streamSubject(id), a.now())
	if err != nil {
		a.log.Warn("a stream token was refused", "request_id", httpapi.RequestIDFrom(ctx),
			"reason", streamRefusalReason(err))
		httpapi.Fail(w, r, problem.NotFound("no stream with that token"))
		return
	}
	start, err := streamStart(r)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if a.streamer == nil || a.blobs == nil {
		// A token was minted with a streamer and this process has none — a
		// restart without ffmpeg. Opaque, like the other refusals.
		a.log.Warn("a stream token was presented to a node with no ffmpeg", "request_id", httpapi.RequestIDFrom(ctx))
		httpapi.Fail(w, r, problem.NotFound("no stream with that token"))
		return
	}
	path, err := a.blobs.SourcePath(ctx, tok.BlobHash)
	if err != nil {
		a.log.Warn("a stream token names a blob this node cannot locate", "request_id", httpapi.RequestIDFrom(ctx),
			"blob", tok.BlobHash, "error", err)
		httpapi.Fail(w, r, problem.NotFound("no stream with that token"))
		return
	}

	// Headers go out with the first byte, not before: a refusal the streamer
	// makes before producing anything (every slot taken) must still be able
	// to answer with a status.
	sw := &streamWriter{w: w, rc: http.NewResponseController(w)}
	err = a.streamer.Stream(ctx, ffmpeg.StreamSpec{
		Source: path, CopyVideo: tok.CopyVideo, CopyAudio: tok.CopyAudio,
		MaxHeight: tok.MaxHeight, Start: start,
	}, sw)
	switch {
	case err == nil, ctx.Err() != nil:
		// Done, or the client left. Nothing to say to either.
	case errors.Is(err, ffmpeg.ErrStreamBusy):
		w.Header().Set("Retry-After", "5")
		httpapi.Fail(w, r, problem.New(http.StatusTooManyRequests, problem.TypeBase+"too-many-streams",
			"Too Many Requests", "every stream slot on this node is in use; try again shortly"))
	case sw.started:
		// ffmpeg failed mid-stream, after the 200 went out. The log has the
		// stderr tail (the streamer wrote it); the client sees a short body.
		a.log.Error("a stream ended early", "request_id", httpapi.RequestIDFrom(ctx),
			"blob", tok.BlobHash, "error", err)
	default:
		a.log.Error("a stream could not start", "request_id", httpapi.RequestIDFrom(ctx),
			"blob", tok.BlobHash, "error", err)
		httpapi.Fail(w, r, problem.Internal())
	}
}

// streamStart reads ?start=, a non-negative number of seconds.
func streamStart(r *http.Request) (float64, error) {
	raw := r.URL.Query().Get("start")
	if raw == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 || f != f {
		return 0, errors.New("start must be a non-negative number of seconds, not " + strconv.Quote(raw))
	}
	return f, nil
}

// streamRefusalReason is the bounded label a refused token is logged under —
// never the token, never the error text.
func streamRefusalReason(err error) string {
	switch {
	case errors.Is(err, errStreamTokenExpired):
		return "expired"
	case errors.Is(err, errStreamTokenSignature):
		return "bad_signature"
	case errors.Is(err, errStreamTokenSubject):
		return "wrong_credential"
	case errors.Is(err, errStreamTokenMalformed):
		return "malformed"
	default:
		return "error"
	}
}

// streamWriter writes the headers with the first byte and flushes every
// write, so a player sees fragments as ffmpeg produces them rather than when
// a buffer fills.
type streamWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
}

func (s *streamWriter) Write(p []byte) (int, error) {
	if !s.started {
		s.started = true
		h := s.w.Header()
		h.Set("Content-Type", streamMIME)
		h.Set("Cache-Control", "no-store")
		h.Set("Accept-Ranges", "none")
		h.Set("X-Accel-Buffering", "no")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Disposition", `inline; filename="stream.mp4"`)
		s.w.WriteHeader(http.StatusOK)
	}
	n, err := s.w.Write(p)
	if err != nil {
		return n, err
	}
	if ferr := s.rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
		return n, ferr
	}
	return n, nil
}
