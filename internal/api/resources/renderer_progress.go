package resources

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
	"github.com/rarebit-one/heyarr-core/internal/renderer"
)

// The progress producer (§68, ADR-0024).
//
// internal/domain/playback/session.go has carried a complete consumption
// state machine since Milestone 2 — created, playing, paused, progress,
// stopped, completed — and until now NOTHING in Heyarr could emit a single
// transition for it except a client calling POST .../transitions by hand. No
// client does. So "continue watching" has been a table that only ever held
// rows saying "created".
//
// A renderer knows exactly where it is. Asking it, on a beat, is the whole
// feature.
//
// # The trap this exists to avoid, learnt the hard way
//
// A Samsung treats a DLNA push as a transient overlay: it plays what it was
// given and then RETURNS TO WHATEVER WAS ON SCREEN BEFORE, resuming it. On the
// night this was written, the television went back to a paused YouTube video
// and continued it.
//
// A poller that simply reads GetPositionInfo and writes the number into the
// session would therefore record the viewer's progress through a YouTube video
// as progress through their film — climbing steadily, for hours, into a field
// that decides where playback resumes tomorrow. The renderer answers honestly;
// the question is wrong.
//
// So every poll checks the URI the renderer says it is playing against the one
// this session handed it. When they differ, the session is ended rather than
// updated. That check is not defensive programming, it is the difference
// between this feature working and silently corrupting itself.

// progressInterval is how often a live session is asked where it is.
//
// Ten seconds is a compromise with a stated bias. Finer would put more SOAP on
// a television for a number nobody reads at that resolution; coarser and a
// resume lands visibly before where someone stopped. Losing up to ten seconds
// of a film on resume is the failure mode, and it is the forgiving one.
const progressInterval = 10 * time.Second

// completionRemainder is how close to the end counts as finished.
//
// Credits are skipped, players stop short, and a renderer that reports 1:29:52
// of 1:30:00 has finished the film by every definition a person uses. Without
// this, nothing is ever completed and everything stays in "continue watching"
// forever — which is worse than the occasional title marked done a few seconds
// early.
const completionRemainder = 30 * time.Second

// PollRendererProgress asks every live renderer-backed session where it is and
// moves it.
//
// Errors are reported per session and never abort the sweep: one television
// that has gone to standby must not stop the speaker downstairs from recording
// where it got to.
func (a *API) PollRendererProgress(ctx context.Context) {
	if a.rendererCache == nil {
		return
	}
	live, err := a.liveRendererSessions(ctx)
	if err != nil {
		a.log.Warn("reading live playback sessions", "error", err)
		return
	}
	for _, s := range live {
		if err := a.pollOne(ctx, s); err != nil {
			a.log.Debug("polling a renderer for progress",
				"session_id", s.SessionID, "renderer", s.DeviceKey, "error", err)
		}
	}
}

// liveSession is a session that might still be playing somewhere.
type liveSession struct {
	SessionID string
	State     playback.State
	// DeviceKey is the renderer's UDN — devices created by discovery are keyed
	// by it (see upsertRendererDevice).
	DeviceKey string
	AssetID   string
	BlobHash  string
}

// liveRendererSessions finds sessions worth asking about.
//
// Only sessions on a device that DISCOVERY created. A device registered by a
// client that plays for itself is not something Heyarr can poll — that client
// reports its own progress — and asking a renderer about it would attribute
// one device's position to another's session.
func (a *API) liveRendererSessions(ctx context.Context) ([]liveSession, error) {
	rows, err := a.reader.QueryContext(ctx, `
		SELECT s.id, s.state, d.device_key, s.asset_id, COALESCE(a.blob_hash, '')
		FROM consumption_sessions s
		JOIN devices d ON d.id = s.device_id
		LEFT JOIN assets a ON a.id = s.asset_id
		WHERE d.platform = ? AND s.state IN (?, ?, ?)`,
		rendererPlatform,
		string(playback.StateCreated), string(playback.StatePlaying), string(playback.StatePaused))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []liveSession
	for rows.Next() {
		var s liveSession
		var state string
		if err := rows.Scan(&s.SessionID, &state, &s.DeviceKey, &s.AssetID, &s.BlobHash); err != nil {
			return nil, err
		}
		s.State = playback.State(state)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (a *API) pollOne(ctx context.Context, s liveSession) error {
	rend, ok := a.rendererCache.get(s.DeviceKey)
	if !ok {
		// Not an error and deliberately not a transition. A renderer missing
		// from the cache has not necessarily stopped — the controller may have
		// restarted, or the sweep may be stale — and ending someone's session
		// on that evidence would lose their place because a process bounced.
		return nil
	}
	ctrl, err := renderer.NewController(a.rendererClient, rend)
	if err != nil {
		return err
	}

	ask, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	state, err := ctrl.State(ask)
	if err != nil {
		// A screen in standby refuses the connection. Same reasoning as above:
		// silence is not evidence that the film ended.
		return err
	}
	pos, posErr := ctrl.Position(ask)

	// THE GUARD. If the renderer has moved on to something that is not ours,
	// its position is a true answer to a question about someone else's
	// content. End the session at the last progress we trust rather than
	// writing that number into it.
	if posErr == nil && s.BlobHash != "" && pos.URI != "" && !playingOurs(pos.URI, s.BlobHash) {
		// Stopped with NO progress argument, which keeps whatever was last
		// recorded rather than overwriting it with a position that belongs to
		// someone else's content.
		return a.transition(ctx, s, playback.TransitionStop, nil)
	}

	switch state {
	case renderer.StatePlaying:
		return a.recordPlaying(ctx, s, pos, posErr)
	case renderer.StatePausedPlayback:
		if s.State == playback.StatePaused {
			return nil
		}
		return a.transition(ctx, s, playback.TransitionPause, progressFrom(pos, posErr))
	case renderer.StateStopped, renderer.StateNoMediaPresent:
		// Finished or abandoned, and the difference matters to a viewer: one
		// should disappear from "continue watching" and the other should be
		// offered again at the place it stopped.
		if posErr == nil && pos.Duration > 0 && pos.Duration-pos.Elapsed <= completionRemainder {
			return a.transition(ctx, s, playback.TransitionComplete, nil)
		}
		return a.transition(ctx, s, playback.TransitionStop, progressFrom(pos, posErr))
	default:
		// TRANSITIONING, or something this version has not met. Waiting is
		// right: a Samsung sits in TRANSITIONING for a second or two while it
		// switches away from whatever app was on screen, and acting on it
		// would end a playback that is about to start.
		return nil
	}
}

// recordPlaying moves a session into playing, and keeps its position current.
func (a *API) recordPlaying(ctx context.Context, s liveSession, pos renderer.Position, posErr error) error {
	progress := progressFrom(pos, posErr)
	switch s.State {
	case playback.StateCreated:
		return a.transition(ctx, s, playback.TransitionStart, progress)
	case playback.StatePaused:
		// Resume, not start. The table allows exactly one of them from each
		// state, and a paused session told to "start" is an illegal
		// transition that would leave the viewer stuck at paused forever.
		return a.transition(ctx, s, playback.TransitionResume, progress)
	}
	if progress == nil {
		// A renderer that reports no position is playing perfectly well and
		// simply will not say where. Recording nothing beats recording zero,
		// which would rewind the session every ten seconds.
		return nil
	}
	if pos.Duration > 0 && pos.Duration-pos.Elapsed <= completionRemainder {
		return a.transition(ctx, s, playback.TransitionComplete, nil)
	}
	return a.transition(ctx, s, playback.TransitionProgress, progress)
}

func (a *API) transition(ctx context.Context, s liveSession, t playback.Transition, progress *playback.Progress) error {
	_, err := a.applySessionTransition(ctx, s.SessionID, t, progress)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, playback.ErrIllegalTransition):
		// The session moved underneath us — someone pressed stop in another
		// client between the read and the write. Their action wins; a poller
		// must not fight a person.
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return nil
	default:
		return err
	}
}

// progressFrom turns a renderer position into session progress, or nil.
//
// Nil when the renderer would not say, which is legal and common. The unit is
// seconds because that is what a renderer reports; ADR-0024's locator-and-unit
// model exists precisely so a page number and a timestamp are not both crammed
// into one float.
func progressFrom(pos renderer.Position, err error) *playback.Progress {
	if err != nil || pos.Elapsed <= 0 {
		return nil
	}
	return &playback.Progress{
		Locator: strconv.FormatInt(int64(pos.Elapsed.Seconds()), 10),
		Unit:    playback.UnitSeconds,
	}
}

// playingOurs reports whether the renderer is on the content this session
// handed it.
//
// The comparison is against the BLOB DIGEST rather than the whole URL, because
// the URL contains a capability that is re-minted on every play and would
// therefore never match on the second poll. The digest is in the capability's
// payload, base64url-encoded, so a substring test over the URL finds it
// without this package needing to verify a token it did not sign.
func playingOurs(uri, blobHash string) bool {
	if blobHash == "" {
		return true
	}
	return strings.Contains(uri, encodedBlobFragment(blobHash))
}

// encodedBlobFragment is how a blob digest appears inside a capability URL.
//
// render.Capability base64url-encodes each field before joining them, so the
// digest is present in the path in exactly this form. Comparing against it,
// rather than against the whole URL, is what makes the guard survive the
// capability being re-minted on every play.
func encodedBlobFragment(blobHash string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(blobHash))
}

// StartProgressBeat polls live renderer sessions until ctx is done.
//
// It runs in the controller because that is where the sessions and the
// renderer cache are, and it is a goroutine rather than a job because it is a
// READ of live external state on a fixed cadence. A durable job per poll would
// put ten seconds of television state into the job table forever (invariant 9
// asks handlers to be idempotent and re-runnable; a position read is neither
// worth persisting nor worth retrying — the next tick is the retry).
func (a *API) StartProgressBeat(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.PollRendererProgress(ctx)
			}
		}
	}()
	a.log.Info("renderer progress beat started", "interval", progressInterval)
}
