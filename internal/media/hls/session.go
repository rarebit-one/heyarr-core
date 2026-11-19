package hls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
)

// Errors a request for part of a presentation can end with.
var (
	// ErrBusy means every stream slot on the node is in use (ADR-0069's cap,
	// shared with the live stream). The caller says 429 and Retry-After.
	ErrBusy = errors.New("hls: every stream slot is in use")
	// ErrNoSegment is a segment number the playlist does not list.
	ErrNoSegment = errors.New("hls: no such segment")
	// ErrNoPlan means the source cannot be cut: no duration, or a copied
	// picture with no keyframes to cut at.
	ErrNoPlan = errors.New("hls: the source cannot be segmented")
	// ErrSegmentLost means a production run went past a segment without
	// making it — the source ended early, or its keyframes moved. A retry
	// would make the same run again, so the answer is a failure, not a wait.
	ErrSegmentLost = errors.New("hls: the segment could not be produced")
	errClosed      = errors.New("hls: the session has ended")
)

// Runner produces a repackage: *ffmpeg.Streamer, whose slots are the node's
// concurrency cap for every kind of stream.
type Runner interface {
	Stream(ctx context.Context, spec ffmpeg.StreamSpec, w io.Writer) error
}

// TimingSource answers what a plan needs from a source: *Indexer.
type TimingSource interface {
	Timing(ctx context.Context, blob, path string, keyframes bool) (Timing, error)
	Warm(blob, path string, keyframes bool)
}

// Options configure a Manager.
type Options struct {
	// Root is the segment cache. The Manager owns it: whatever is in it at
	// start is a previous process's and is removed.
	Root   string
	Runner Runner
	Index  TimingSource
	Logger *slog.Logger
	// Now is injected so idle cleanup is testable without sleeping.
	Now func() time.Time
	// Readahead is how many segments a run may get ahead of the furthest one
	// asked for before it stops reading ffmpeg — which stops ffmpeg, without
	// killing it, until the player catches up.
	Readahead int
	// KeepBehind is how many segments behind the playhead stay on disk, for a
	// player that rewinds a few seconds.
	KeepBehind int
	// ProducerIdle stops a run nobody has asked anything of for this long; a
	// paused player gives its stream slot back. SessionIdle forgets the
	// session and its cache.
	ProducerIdle time.Duration
	SessionIdle  time.Duration
}

// Defaults for the zero Options fields.
const (
	DefaultReadahead    = 4
	DefaultKeepBehind   = 3
	DefaultProducerIdle = 30 * time.Second
	DefaultSessionIdle  = 10 * time.Minute
	// retargetWindow is how far ahead of a run's position a requested
	// segment may be and still be waited for rather than started afresh. A
	// copy reaches two segments ahead in well under a second; a re-encode
	// takes longer, but no longer than a restart and its first segment.
	retargetWindow = 2
	// maxBoxPayload bounds a box the segmenter holds in memory (ftyp, moov,
	// moof). Media data is streamed to disk, never held.
	maxBoxPayload = 16 << 20
)

// Manager holds the live presentations, one per planned stream token.
type Manager struct {
	root   string
	runner Runner
	index  TimingSource
	log    *slog.Logger
	now    func() time.Time

	readahead, keepBehind     int
	producerIdle, sessionIdle time.Duration

	mu       sync.Mutex
	sessions map[string]*Session
	// opened numbers every session the Manager starts, so each has its own
	// directory: one forgotten while a player resumes is still being removed
	// when the same id opens again, and must not take the new one's cache.
	opened uint64
}

// New builds a Manager and clears its cache directory.
func New(opts Options) (*Manager, error) {
	if opts.Root == "" || opts.Runner == nil || opts.Index == nil {
		return nil, errors.New("hls: a cache directory, a runner and a timing source are required")
	}
	if err := os.MkdirAll(opts.Root, 0o750); err != nil {
		return nil, fmt.Errorf("hls: creating the segment cache: %w", err)
	}
	entries, err := os.ReadDir(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("hls: reading the segment cache: %w", err)
	}
	// Segments are derived bytes nothing else references (not blobs, not in
	// the CAS); a previous process's are unreachable once it is gone.
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(opts.Root, e.Name())); err != nil {
			return nil, fmt.Errorf("hls: clearing the segment cache: %w", err)
		}
	}
	m := &Manager{
		root: opts.Root, runner: opts.Runner, index: opts.Index, now: opts.Now,
		readahead: opts.Readahead, keepBehind: opts.KeepBehind,
		producerIdle: opts.ProducerIdle, sessionIdle: opts.SessionIdle,
		sessions: map[string]*Session{},
	}
	m.log = opts.Logger
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	m.log = m.log.With("component", "hls")
	if m.now == nil {
		m.now = time.Now
	}
	if m.readahead <= 0 {
		m.readahead = DefaultReadahead
	}
	if m.keepBehind <= 0 {
		m.keepBehind = DefaultKeepBehind
	}
	if m.producerIdle <= 0 {
		m.producerIdle = DefaultProducerIdle
	}
	if m.sessionIdle <= 0 {
		m.sessionIdle = DefaultSessionIdle
	}
	return m, nil
}

// Source is one planned repackage: the bytes and what to do to them. The
// spec's Start and Segmented are the Manager's to set.
type Source struct {
	// Blob is the source's digest, which is what its timing is remembered by.
	Blob string
	Spec ffmpeg.StreamSpec
}

// Warm starts reading what a plan for src will need, so the first playlist
// request does not pay for it.
func (m *Manager) Warm(src Source) { m.index.Warm(src.Blob, src.Spec.Source, src.Spec.CopyVideo) }

// Open returns the session id names, starting it if it is not running. id
// must be filesystem-safe; the caller derives it from the stream token.
func (m *Manager) Open(ctx context.Context, id string, src Source) (*Session, error) {
	m.mu.Lock()
	if s, ok := m.sessions[id]; ok {
		m.mu.Unlock()
		s.touch()
		return s, nil
	}
	m.mu.Unlock()

	timing, err := m.index.Timing(ctx, src.Blob, src.Spec.Source, src.Spec.CopyVideo)
	if err != nil {
		return nil, err
	}
	var segs []Segment
	if src.Spec.CopyVideo {
		segs = PlanKeyframes(timing.Keyframes, timing.Start+timing.Duration, SegmentSeconds)
	} else {
		segs = PlanUniform(timing.Duration, SegmentSeconds)
	}
	if len(segs) == 0 {
		return nil, ErrNoPlan
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.touch()
		return s, nil
	}
	m.opened++
	dir := filepath.Join(m.root, id+"."+strconv.FormatUint(m.opened, 10))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("hls: creating a session directory: %w", err)
	}
	s := &Session{
		m: m, id: id, dir: dir, src: src, segs: segs, keyframes: timing.Keyframes,
		playlist: Playlist(segs), state: make([]segState, len(segs)),
		changed: make(chan struct{}), lastUse: m.now(),
	}
	m.sessions[id] = s
	m.log.Info("hls session opened", "session", id, "segments", len(segs), "copy_video", src.Spec.CopyVideo)
	return s, nil
}

// Active is how many sessions have a production run going.
func (m *Manager) Active() int {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	n := 0
	for _, s := range sessions {
		s.mu.Lock()
		if s.prod != nil && !s.prod.exited {
			n++
		}
		s.mu.Unlock()
	}
	return n
}

// Sweep stops production nobody is consuming and forgets sessions nobody has
// touched. Run calls it on a tick; tests call it with the clock they set.
func (m *Manager) Sweep(now time.Time) {
	m.mu.Lock()
	var ended []*Session
	for id, s := range m.sessions {
		s.mu.Lock()
		idle := now.Sub(s.lastUse)
		if s.prod != nil && !s.prod.cancelled && idle >= m.producerIdle {
			m.log.Info("hls run stopped: nobody is asking", "session", id, "idle", idle)
			s.prod.stop()
		}
		if idle >= m.sessionIdle {
			s.closed = true
			s.broadcastLocked()
			delete(m.sessions, id)
			ended = append(ended, s)
		}
		s.mu.Unlock()
	}
	m.mu.Unlock()
	for _, s := range ended {
		m.log.Info("hls session ended", "session", s.id)
		s.remove()
	}
}

// Run sweeps every interval until ctx ends, then stops every run and clears
// the cache.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.Close()
			return
		case <-t.C:
			m.Sweep(m.now())
		}
	}
}

// Close ends every session.
func (m *Manager) Close() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		s.closed = true
		s.broadcastLocked()
		s.mu.Unlock()
		s.remove()
	}
}

type segState int

const (
	segAbsent segState = iota
	segReady
	segLost
)

// Session is one client's presentation of one planned repackage.
type Session struct {
	m         *Manager
	id, dir   string
	src       Source
	segs      []Segment
	keyframes []float64
	playlist  string

	mu sync.Mutex
	// changed is closed and replaced whenever anything a waiter could be
	// waiting for happens.
	changed chan struct{}
	state   []segState
	init    []byte
	// want is the playhead as the requests reveal it: the furthest segment
	// asked for, reset by a seek backwards. Readahead and eviction are
	// measured from it.
	want    int
	lastUse time.Time
	prod    *producer
	gen     int
	closed  bool
}

// producer is one ffmpeg run, from one segment onwards.
type producer struct {
	gen       int
	from      int
	cursor    int // the segment being written now
	cancel    context.CancelFunc
	cancelled bool
	exited    bool
	done      chan struct{}
	err       error
}

func (p *producer) stop() {
	p.cancelled = true
	p.cancel()
}

// covers reports whether the run will reach segment k soon enough to wait.
func (p *producer) covers(k int) bool {
	return !p.exited && !p.cancelled && k >= p.cursor && k <= p.cursor+retargetWindow
}

// Segments is the plan.
func (s *Session) Segments() []Segment { return s.segs }

// Cached lists the segments on disk now, ascending.
func (s *Session) Cached() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for i, st := range s.state {
		if st == segReady {
			out = append(out, i)
		}
	}
	return out
}

// Position is the segment the running production is writing or paused
// before, or -1 when nothing is running.
func (s *Session) Position() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prod == nil || s.prod.exited || s.prod.cancelled {
		return -1
	}
	return s.prod.cursor
}

// Playlist is the media playlist.
func (s *Session) Playlist() string {
	s.touch()
	return s.playlist
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastUse = s.m.now()
	s.mu.Unlock()
}

func (s *Session) broadcastLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Init returns the init segment, starting production if nothing has made it.
func (s *Session) Init(ctx context.Context) ([]byte, error) {
	var waited *producer
	for {
		s.mu.Lock()
		s.lastUse = s.m.now()
		if s.init != nil {
			b := s.init
			s.mu.Unlock()
			return b, nil
		}
		if s.closed {
			s.mu.Unlock()
			return nil, errClosed
		}
		p := s.prod
		if p != nil && p == waited && p.exited && p.err != nil {
			s.mu.Unlock()
			return nil, p.err
		}
		if p == nil || p.exited || p.cancelled {
			p = s.startLocked(max(s.want, 0))
		}
		waited = p
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

// Segment opens segment k, producing it if it is not cached: waiting for the
// run already heading there, or starting one at k.
func (s *Session) Segment(ctx context.Context, k int) (*os.File, error) {
	if k < 0 || k >= len(s.segs) {
		return nil, ErrNoSegment
	}
	var waited *producer
	for {
		s.mu.Lock()
		s.lastUse = s.m.now()
		s.noteWantLocked(k)
		switch s.state[k] {
		case segReady:
			f, err := os.Open(s.segmentPath(k))
			s.mu.Unlock()
			return f, err
		case segLost:
			s.mu.Unlock()
			return nil, ErrSegmentLost
		}
		if s.closed {
			s.mu.Unlock()
			return nil, errClosed
		}
		p := s.prod
		if p != nil && p == waited && p.exited && p.err != nil {
			s.mu.Unlock()
			return nil, p.err
		}
		if p == nil || !p.covers(k) {
			p = s.startLocked(k)
		}
		waited = p
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

// noteWantLocked moves the playhead: forward with every request, back only
// when a request is further behind it than a player fetching segments out of
// order would be — that is a seek.
func (s *Session) noteWantLocked(k int) {
	if k > s.want || k < s.want-retargetWindow {
		s.want = k
		s.broadcastLocked()
	}
}

func (s *Session) segmentPath(k int) string {
	return filepath.Join(s.dir, strconv.Itoa(k)+".m4s")
}

// startLocked starts a run at segment k, stopping the one before it. The new
// run waits for the old to exit, so two never hold a slot for one session.
func (s *Session) startLocked(k int) *producer {
	old := s.prod
	if old != nil && !old.exited {
		old.stop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.gen++
	p := &producer{gen: s.gen, from: k, cursor: k, cancel: cancel, done: make(chan struct{})}
	s.prod = p
	if old != nil && !old.exited {
		s.m.log.Info("hls run retargeted", "session", s.id, "from", old.cursor, "to", k)
	}
	go s.run(ctx, p, old)
	return p
}

// spec is the ffmpeg run that starts at segment k.
func (s *Session) spec(k int) ffmpeg.StreamSpec {
	spec := s.src.Spec
	spec.Start = 0
	seg := &ffmpeg.Segmented{KeyframeSeconds: SegmentSeconds}
	if k > 0 {
		seg.From = s.segs[k].Start
		if spec.CopyVideo {
			// Aim between the boundary keyframe and the next one, so a time
			// rounded to the microsecond on its way out of ffprobe still has
			// the boundary as the last keyframe at or before the seek.
			gap := s.segs[k].Duration
			for _, kf := range s.keyframes {
				if kf > seg.From+locateTolerance {
					gap = kf - seg.From
					break
				}
			}
			seg.SeekPast = min(gap/2, 0.25)
		}
	}
	spec.Segmented = seg
	return spec
}

// run is one production run: ffmpeg into the segmenter until the film ends,
// the run is stopped, or something fails.
func (s *Session) run(ctx context.Context, p *producer, prev *producer) {
	if prev != nil {
		<-prev.done
	}
	pr, pw := io.Pipe()
	streamDone := make(chan error, 1)
	go func() {
		err := s.m.runner.Stream(ctx, s.spec(p.from), pw)
		if err == nil {
			err = io.EOF
		}
		pw.CloseWithError(err)
		streamDone <- err
	}()
	segErr := s.cut(ctx, p, pr)
	stopped := ctx.Err() != nil
	if segErr != nil {
		// The segmenter gave up first: unblock the writer and kill ffmpeg,
		// which would otherwise run on into a pipe nobody reads.
		_ = pr.CloseWithError(segErr)
		p.cancel()
	} else {
		_ = pr.Close()
	}
	streamErr := <-streamDone
	p.cancel()

	var err error
	switch {
	case stopped:
		// Stopped on purpose: retargeted, idle or closed.
	case errors.Is(streamErr, ffmpeg.ErrStreamBusy):
		err = ErrBusy
	case streamErr != nil && !errors.Is(streamErr, io.EOF):
		err = streamErr
	case segErr != nil:
		err = segErr
	}
	s.mu.Lock()
	p.exited, p.err = true, err
	if err == nil && !stopped {
		// The film ended. Anything this run should have reached and did not
		// is lost, not pending: another run would end the same way.
		for i := max(p.cursor, p.from); i < len(s.state); i++ {
			if s.state[i] == segAbsent {
				s.state[i] = segLost
			}
		}
	}
	s.broadcastLocked()
	s.mu.Unlock()
	close(p.done)
	if err != nil && !errors.Is(err, ErrBusy) {
		s.m.log.Warn("hls run failed", "session", s.id, "from", p.from, "error", err)
	}
}

// cut reads ffmpeg's fragmented MP4 and files it as the init segment and
// numbered media segments.
func (s *Session) cut(ctx context.Context, p *producer, r io.Reader) error {
	var (
		initBytes []byte
		tracks    map[uint32]track
		cur       = -1 // the segment being written; -1 is none (discarding)
		opened    bool
		out       *os.File
		tmp       string
	)
	finish := func() error {
		if out == nil {
			return nil
		}
		err := out.Close()
		out = nil
		if err == nil {
			err = os.Rename(tmp, s.segmentPath(cur))
		}
		if err != nil {
			_ = os.Remove(tmp)
			return err
		}
		s.mu.Lock()
		s.state[cur] = segReady
		s.evictLocked()
		s.broadcastLocked()
		s.mu.Unlock()
		return nil
	}
	defer func() {
		if out != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
		}
	}()
	for {
		h, raw, err := readBoxHeader(r)
		if errors.Is(err, io.EOF) {
			return finish()
		}
		if err != nil {
			return err
		}
		body := h.size - h.header
		switch h.typ {
		case "ftyp", "moov", "moof":
			if body > maxBoxPayload {
				return fmt.Errorf("%w: a %d-byte %s", errBox, body, h.typ)
			}
			payload := make([]byte, body)
			if _, err := io.ReadFull(r, payload); err != nil {
				return errBox
			}
			if h.typ != "moof" {
				initBytes = append(append(initBytes, raw...), payload...)
				if h.typ == "moov" {
					if tracks, err = parseMoov(payload); err != nil {
						return err
					}
					s.setInit(initBytes)
				}
				continue
			}
			if tracks == nil {
				return fmt.Errorf("%w: a fragment before the init segment", errBox)
			}
			start, ok, err := videoStart(payload, tracks)
			if err != nil {
				return err
			}
			if ok {
				idx := Locate(s.segs, start-ffmpeg.SegmentTimelineOffset)
				if idx > cur {
					if err := finish(); err != nil {
						return err
					}
					if idx < p.from {
						// The demuxer landed on an earlier keyframe than the
						// boundary; those fragments are someone else's.
						cur = -1
					} else {
						switch {
						case cur >= 0 && idx > cur+1:
							s.markLost(cur+1, idx)
						case !opened && idx > p.from:
							// The run began past the segment it was started
							// for; restarting it would begin there again.
							s.markLost(p.from, idx)
						}
						opened = true
						cur = idx
						if err := s.waitWindow(ctx, p, idx); err != nil {
							return err
						}
						tmp = s.segmentPath(idx) + ".part" + strconv.Itoa(p.gen)
						// #nosec G304 -- the session's own directory and a
						// name made of two integers.
						if out, err = os.Create(tmp); err != nil {
							return err
						}
					}
				}
			}
			if out != nil {
				if _, err := out.Write(raw); err != nil {
					return err
				}
				if _, err := out.Write(payload); err != nil {
					return err
				}
			}
		default:
			// mdat goes where its moof went; anything else (the mfra the
			// muxer appends at the end) is dropped.
			dst := io.Discard
			if h.typ == "mdat" && out != nil {
				if _, err := out.Write(raw); err != nil {
					return err
				}
				dst = out
			}
			if _, err := io.CopyN(dst, r, body); err != nil {
				return errBox
			}
		}
	}
}

func (s *Session) setInit(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.init == nil {
		s.init = append([]byte(nil), b...)
		s.broadcastLocked()
		return
	}
	if string(s.init) != string(b) {
		// Every run of one plan should write the same init; one that did not
		// is a toolchain surprise worth knowing about. The first is kept: a
		// player has already been given it.
		s.m.log.Warn("an hls run wrote a different init segment", "session", s.id)
	}
}

func (s *Session) markLost(from, to int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := from; i < to; i++ {
		if s.state[i] == segAbsent {
			s.state[i] = segLost
		}
	}
	s.m.log.Warn("an hls run skipped segments", "session", s.id, "from", from, "to", to-1)
	s.broadcastLocked()
}

// waitWindow holds the run before it writes segment idx until the playhead is
// within readahead of it. Holding it is not reading ffmpeg's output, so ffmpeg
// blocks on a full pipe and spends nothing until the player catches up.
func (s *Session) waitWindow(ctx context.Context, p *producer, idx int) error {
	for {
		s.mu.Lock()
		if p.cursor != idx {
			p.cursor = idx
			s.broadcastLocked()
		}
		if idx <= s.want+s.m.readahead || s.closed {
			s.broadcastLocked()
			s.mu.Unlock()
			return nil
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// evictLocked deletes cached segments the playhead has left behind, and any
// left far ahead of it by a run a seek abandoned.
func (s *Session) evictLocked() {
	for i, st := range s.state {
		if st != segReady {
			continue
		}
		if i < s.want-s.m.keepBehind || i > s.want+s.m.readahead+retargetWindow {
			if err := os.Remove(s.segmentPath(i)); err == nil || os.IsNotExist(err) {
				s.state[i] = segAbsent
			}
		}
	}
}

// remove stops the run and deletes the session's cache once it has exited.
func (s *Session) remove() {
	s.mu.Lock()
	p := s.prod
	if p != nil && !p.exited {
		p.stop()
	}
	s.mu.Unlock()
	if p != nil {
		<-p.done
	}
	if err := os.RemoveAll(s.dir); err != nil {
		s.m.log.Warn("removing an hls session's cache", "session", s.id, "error", err)
	}
}
