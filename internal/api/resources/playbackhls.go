package resources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
	"github.com/rarebit-one/heyarr-core/internal/media/hls"
)

// The seekable presentation of the repackage (ADR-0126).
//
// A `stream` plan's bytes are also offered as HLS: a VOD playlist listing the
// whole film, an init segment, and numbered fMP4 segments made on demand. A
// Cast receiver, a television or a player that seeks by itself fetches
// GET /render/hls/{capability}/index.m3u8 and the segments it names, with no
// credential — the capability in the path is the authority, as on the render
// route (ADR-0040), which is why these routes are a public mount under the
// same /render prefix: the plain-HTTP render listener (ADR-0079) serves them,
// and the access log redacts them, with no second rule for either.
//
// The live stream at /api/v1/playback/stream/{token} is unchanged, and still
// what `url` names: a client that does not know `hls_url` plays as it did.

// hlsMIME is what a playlist is served as (RFC 8216 §4).
const hlsMIME = "application/vnd.apple.mpegurl"

// hlsPrefix is where a presentation lives, relative like content_url.
const hlsPrefix = httpapi.RenderPrefix + "/hls/"

// hlsPlaylistURL is the playlist a plan advertises for a capability.
func hlsPlaylistURL(capability string) string { return hlsPrefix + capability + "/index.m3u8" }

// HLSPresenter holds the live presentations: *hls.Manager.
type HLSPresenter interface {
	Open(ctx context.Context, id string, src hls.Source) (*hls.Session, error)
	Warm(src hls.Source)
}

// MountHLS registers the presentation routes on an UNAUTHENTICATED router,
// like the render route. They are mounted on every node, as the stream route
// is: one that cannot present never advertises a playlist, and answers a
// capability presented to it anyway with the same opaque 404 as a bad one.
func (a *API) MountHLS(r chi.Router) {
	r.Route(hlsPrefix+"{token}", func(r chi.Router) {
		r.Use(hlsCORS)
		r.Get("/index.m3u8", a.hlsPlaylist)
		r.Head("/index.m3u8", a.hlsPlaylist)
		r.Get("/init.mp4", a.hlsInit)
		r.Head("/init.mp4", a.hlsInit)
		r.Get("/{segment}", a.hlsSegment)
		r.Head("/{segment}", a.hlsSegment)
	})
}

// hlsCORS lets a player whose page is on another origin read the
// presentation. A Cast receiver runs on Google's origin and fetches the
// playlist and segments with XHR, which the browser refuses to hand it
// without these. Any origin is allowed: the capability, not the origin, is
// the authority, and no cookie or credential rides these requests.
func hlsCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Range, Accept, Origin, Content-Type")
		h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
		h.Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hlsSessionID names a capability's session on disk: a digest of the
// capability, so the cache directory never holds the secret and one plan is
// one session.
func hlsSessionID(capability string) string {
	sum := sha256.Sum256([]byte(capability))
	return hex.EncodeToString(sum[:16])
}

// hlsSession verifies the capability and opens its presentation, answering
// the request itself when it cannot.
//
// Every refusal of the capability — a bad signature, an expiry, a blob this
// node no longer holds — is the same opaque 404, with a bounded reason in the
// log, as on the stream route.
func (a *API) hlsSession(w http.ResponseWriter, r *http.Request) (*hls.Session, bool) {
	ctx := r.Context()
	capability := chi.URLParam(r, "token")
	tok, err := verifyHLSToken(a.hlsKey, capability, a.now())
	if err != nil {
		a.log.Warn("an hls capability was refused", "request_id", httpapi.RequestIDFrom(ctx),
			"reason", streamRefusalReason(err))
		hlsProblem(w, r, problem.NotFound("no presentation with that capability"))
		return nil, false
	}
	if a.hls == nil || a.blobs == nil {
		// A capability minted by a node that could present, shown to this
		// process, which cannot: a restart without the toolchain.
		a.log.Warn("an hls capability was presented to a node that cannot present", "request_id", httpapi.RequestIDFrom(ctx))
		hlsProblem(w, r, problem.NotFound("no presentation with that capability"))
		return nil, false
	}
	path, err := a.blobs.SourcePath(ctx, tok.BlobHash)
	if err != nil {
		a.log.Warn("an hls capability names a blob this node cannot locate", "request_id", httpapi.RequestIDFrom(ctx),
			"blob", tok.BlobHash, "error", err)
		hlsProblem(w, r, problem.NotFound("no presentation with that capability"))
		return nil, false
	}
	s, err := a.hls.Open(ctx, hlsSessionID(capability), hls.Source{Blob: tok.BlobHash, Spec: ffmpeg.StreamSpec{
		Source: path, CopyVideo: tok.CopyVideo, CopyAudio: tok.CopyAudio,
		MaxHeight: tok.MaxHeight, ToneMap: tok.ToneMap, AudioTrack: tok.AudioTrack,
	}})
	switch {
	case err == nil:
		return s, true
	case ctx.Err() != nil:
		return nil, false
	case errors.Is(err, hls.ErrNoPlan):
		// The live stream still plays; this source just cannot be cut.
		a.log.Warn("an hls presentation could not be planned", "request_id", httpapi.RequestIDFrom(ctx),
			"blob", tok.BlobHash, "error", err)
		hlsProblem(w, r, problem.NotFound("this source cannot be presented as segments; play the stream url instead"))
	default:
		a.log.Error("opening an hls presentation", "request_id", httpapi.RequestIDFrom(ctx),
			"blob", tok.BlobHash, "error", err)
		hlsProblem(w, r, problem.Internal())
	}
	return nil, false
}

func (a *API) hlsPlaylist(w http.ResponseWriter, r *http.Request) {
	s, ok := a.hlsSession(w, r)
	if !ok {
		return
	}
	body := s.Playlist()
	h := w.Header()
	h.Set("Content-Type", hlsMIME)
	// The playlist of one capability never changes, but a player re-reading
	// it after a re-plan must not be handed a cached copy of the old one.
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		// #nosec G705 -- a playlist of numbers and fixed tags, served as
		// application/vnd.apple.mpegurl with nosniff; nothing in it is input.
		_, _ = w.Write([]byte(body))
	}
}

func (a *API) hlsInit(w http.ResponseWriter, r *http.Request) {
	s, ok := a.hlsSession(w, r)
	if !ok {
		return
	}
	b, err := s.Init(r.Context())
	if err != nil {
		a.hlsFail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "video/mp4")
	h.Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, "init.mp4", time.Time{}, strings.NewReader(string(b)))
}

func (a *API) hlsSegment(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "segment")
	digits, ok := strings.CutSuffix(name, ".m4s")
	n, err := strconv.Atoi(digits)
	if !ok || err != nil || n < 0 || strconv.Itoa(n) != digits {
		hlsProblem(w, r, problem.NotFound("no such segment"))
		return
	}
	s, ok := a.hlsSession(w, r)
	if !ok {
		return
	}
	f, err := s.Segment(r.Context(), n)
	if err != nil {
		a.hlsFail(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()
	h := w.Header()
	h.Set("Content-Type", "video/mp4")
	h.Set("Cache-Control", "private, max-age=3600")
	// Ranges and HEAD come from ServeContent: the segment is a whole file by
	// now, unlike the live stream.
	http.ServeContent(w, r, name, time.Time{}, f)
}

// hlsFail answers a request the presentation could not satisfy.
func (a *API) hlsFail(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	switch {
	case ctx.Err() != nil:
		// The player went away.
	case errors.Is(err, hls.ErrBusy), errors.Is(err, ffmpeg.ErrStreamBusy):
		w.Header().Set("Retry-After", "5")
		hlsProblem(w, r, problem.New(http.StatusTooManyRequests, problem.TypeBase+"too-many-streams",
			"Too Many Requests", "every stream slot on this node is in use; try again shortly"))
	case errors.Is(err, hls.ErrNoSegment):
		hlsProblem(w, r, problem.NotFound("no such segment"))
	default:
		a.log.Error("producing an hls segment", "request_id", httpapi.RequestIDFrom(ctx), "error", err)
		hlsProblem(w, r, problem.Internal())
	}
}

// offerHLS adds the presentation of a stream plan to it, and starts reading
// what the playlist will need — for a copied picture, every keyframe in the
// file — so the player's first request usually finds it ready. A node that
// cannot present, or a capability that will not sign, leaves the plan as it
// was: the live stream is the answer either way.
func (a *API) offerHLS(ctx context.Context, out *PlanResponse, tok streamToken) {
	if a.hls == nil || a.blobs == nil {
		return
	}
	path, err := a.blobs.SourcePath(ctx, tok.BlobHash)
	if err != nil {
		return
	}
	capability, err := tok.signHLS(a.hlsKey)
	if err != nil {
		a.log.Error("signing an hls capability", "request_id", httpapi.RequestIDFrom(ctx), "error", err)
		return
	}
	out.HLSURL, out.HLSMIME = hlsPlaylistURL(capability), hlsMIME
	a.hls.Warm(hls.Source{Blob: tok.BlobHash, Spec: ffmpeg.StreamSpec{Source: path, CopyVideo: tok.CopyVideo}})
}

// hlsProblem writes a problem with a constant instance. problem.Write would
// otherwise fill it from the request path, which on these routes CONTAINS the
// capability — handing the secret back in a body intermediaries log, on a
// route whose access log is redacted for exactly that reason (the render
// route's stance, ADR-0040).
func hlsProblem(w http.ResponseWriter, r *http.Request, p *problem.Problem) {
	httpapi.Fail(w, r, p.WithInstance(strings.TrimSuffix(hlsPrefix, "/")))
}
