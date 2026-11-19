package hls_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
	"github.com/rarebit-one/heyarr-core/internal/media/hls"
)

// These run a real ffmpeg and ffprobe: the point is the bytes a player gets.
// The Linux CI leg has the pinned toolchain and fails the build if anything
// under internal/media skips there; the bare macOS leg skips them.

func toolchain(t *testing.T) (ffmpegPath, ffprobePath string) {
	t.Helper()
	dir := os.Getenv("HEYARR_TEST_TOOLCHAIN_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "..", ".toolchain", "bin")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(abs, "ffmpeg")); err != nil {
		if p, lookErr := exec.LookPath("ffmpeg"); lookErr == nil {
			probe, _ := exec.LookPath("ffprobe")
			return p, probe
		}
		t.Skip("no ffmpeg available; run scripts/toolchain.sh")
	}
	return filepath.Join(abs, "ffmpeg"), filepath.Join(abs, "ffprobe")
}

// filmKeyframes are where the fixture's picture has keyframes: irregular on
// purpose for the first forty seconds, so a boundary that is not a real
// keyframe has nowhere to hide, then every four seconds to the end.
var filmKeyframes = func() []float64 {
	kf := []float64{0, 2.5, 4, 9.5, 11, 13.25, 19, 20, 26, 33, 37}
	for k := 40.0; k < filmSeconds; k += 4 {
		kf = append(kf, k)
	}
	return kf
}()

const filmSeconds = 120.0

// film is a two-minute source: H.264 with B-frames and those keyframes, and an
// AC-3 track — the shape of the remux a phone cannot hear — in Matroska.
func film(t *testing.T, ffmpegPath string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "film.mkv")
	kf := make([]string, len(filmKeyframes))
	for i, k := range filmKeyframes {
		kf[i] = strconv.FormatFloat(k, 'f', -1, 64)
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", strconv.FormatFloat(filmSeconds, 'f', -1, 64),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-bf", "2",
		"-g", "1000", "-sc_threshold", "0", "-force_key_frames", strings.Join(kf, ","),
		"-c:a", "ac3", "-b:a", "128k", out,
	}
	if b, err := exec.CommandContext(t.Context(), ffmpegPath, args...).CombinedOutput(); err != nil {
		t.Fatalf("generating the film: %v\n%s", err, b)
	}
	return out
}

// recorder is a Runner that remembers every run it was asked for.
type recorder struct {
	inner hls.Runner
	mu    sync.Mutex
	specs []ffmpeg.StreamSpec
}

func (r *recorder) Stream(ctx context.Context, spec ffmpeg.StreamSpec, w io.Writer) error {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	return r.inner.Stream(ctx, spec, w)
}

func (r *recorder) runs() []ffmpeg.StreamSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ffmpeg.StreamSpec(nil), r.specs...)
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

type rig struct {
	m       *hls.Manager
	rec     *recorder
	root    string
	clock   *clock
	src     string
	ffprobe string
	ffmpeg  string
}

func newRig(t *testing.T, slots int) *rig {
	t.Helper()
	ffmpegPath, ffprobePath := toolchain(t)
	streamer, err := ffmpeg.NewStreamer(ffmpeg.StreamerOptions{FFmpegPath: ffmpegPath, MaxConcurrent: slots})
	if err != nil {
		t.Fatal(err)
	}
	index, err := hls.NewIndexer(ffprobePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{
		rec: &recorder{inner: streamer}, root: filepath.Join(t.TempDir(), "hls"),
		clock: &clock{now: time.Unix(1_800_000_000, 0)}, src: film(t, ffmpegPath),
		ffprobe: ffprobePath, ffmpeg: ffmpegPath,
	}
	r.m, err = hls.New(hls.Options{Root: r.root, Runner: r.rec, Index: index, Now: r.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.m.Close)
	return r
}

func (r *rig) open(t *testing.T, id string, copyVideo bool) *hls.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	s, err := r.m.Open(ctx, id, hls.Source{
		Blob: "blob-" + strconv.FormatBool(copyVideo),
		Spec: ffmpeg.StreamSpec{Source: r.src, CopyVideo: copyVideo},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fetch(t *testing.T, s *hls.Session, k int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	f, err := s.Segment(ctx, k)
	if err != nil {
		t.Fatalf("segment %d: %v", k, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func initSegment(t *testing.T, s *hls.Session) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	b, err := s.Init(ctx)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	return b
}

// packet is one ffprobe packet line.
type packet struct {
	stream   int
	pts, dur float64
	key      bool
}

func packets(t *testing.T, ffprobePath string, parts ...[]byte) []packet {
	t.Helper()
	p := filepath.Join(t.TempDir(), "joined.mp4")
	var all []byte
	for _, b := range parts {
		all = append(all, b...)
	}
	if err := os.WriteFile(p, all, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), ffprobePath, "-v", "error",
		"-show_entries", "packet=stream_index,pts_time,duration_time,flags", "-of", "compact=p=0:nk=1", p).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var pkts []packet
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "|")
		if len(f) < 4 {
			continue
		}
		idx, _ := strconv.Atoi(f[0])
		pts, err1 := strconv.ParseFloat(f[1], 64)
		dur, err2 := strconv.ParseFloat(f[2], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		pkts = append(pkts, packet{stream: idx, pts: pts, dur: dur, key: strings.Contains(f[3], "K")})
	}
	return pkts
}

func ofStream(pkts []packet, stream int) []packet {
	var out []packet
	for _, p := range pkts {
		if p.stream == stream {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pts < out[j].pts })
	return out
}

// The tests below share one shape: the presentation as a player would fetch
// it, checked against ffprobe's reading of the bytes. Stream 0 is the picture
// and stream 1 the sound, because the run maps them in that order.

func TestAPresentationIsTheWholeFilmCutAtKeyframes(t *testing.T) {
	for _, copyVideo := range []bool{true, false} {
		name := "video re-encoded"
		if copyVideo {
			name = "video copied"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t, 2)
			s := r.open(t, "whole", copyVideo)
			segs := s.Segments()

			// The playlist is the film: its durations add up to it.
			if total := sum(segs); math.Abs(total-filmSeconds) > 0.1 {
				t.Errorf("playlist durations sum to %.3fs, want the film's %.0fs", total, filmSeconds)
			}
			if copyVideo {
				// A copy is cut only where the source has keyframes.
				for _, seg := range segs {
					if !containsNear(filmKeyframes, seg.Start) {
						t.Errorf("a boundary at %.3fs is not a source keyframe %v", seg.Start, filmKeyframes)
					}
				}
			}

			init := initSegment(t, s)
			var all [][]byte
			all = append(all, init)
			for k, seg := range segs {
				b := fetch(t, s, k)
				all = append(all, b)
				// Every segment opens on a keyframe AT its boundary: a player
				// that seeks there decodes from its first byte.
				first := ofStream(packets(t, r.ffprobe, init, b), 0)
				if len(first) == 0 || !first[0].key {
					t.Fatalf("segment %d does not open on a keyframe", k)
				}
				if got := first[0].pts - ffmpeg.SegmentTimelineOffset; math.Abs(got-seg.Start) > 0.002 {
					t.Errorf("segment %d opens at %.4fs, its boundary is %.4fs", k, got, seg.Start)
				}
			}

			// Joined, the segments are one continuous film: no gap and no
			// overlap in the picture or the sound at any boundary.
			pkts := packets(t, r.ffprobe, all...)
			video, audio := ofStream(pkts, 0), ofStream(pkts, 1)
			assertContinuous(t, "video", video, 1.0/24)
			assertContinuous(t, "audio", audio, 0)
			vEnd := video[len(video)-1].pts + video[len(video)-1].dur - video[0].pts
			aEnd := audio[len(audio)-1].pts + audio[len(audio)-1].dur - audio[0].pts
			if math.Abs(vEnd-filmSeconds) > 0.1 || math.Abs(aEnd-filmSeconds) > 0.1 {
				t.Errorf("joined segments play %.3fs of picture and %.3fs of sound, want %.0fs of each", vEnd, aEnd, filmSeconds)
			}
			if math.Abs(video[0].pts-audio[0].pts) > 0.05 {
				t.Errorf("picture starts at %.3fs and sound at %.3fs", video[0].pts, audio[0].pts)
			}
			// One run made all of it.
			if runs := r.rec.runs(); len(runs) != 1 {
				t.Errorf("sequential playback took %d ffmpeg runs, want 1", len(runs))
			}
		})
	}
}

func containsNear(xs []float64, v float64) bool {
	for _, x := range xs {
		if math.Abs(x-v) < 0.002 {
			return true
		}
	}
	return false
}

// assertContinuous checks each packet starts where the one before it ended.
// frame, when non-zero, is the most a picture may step between packets — the
// packet durations of a stream with B-frames are not always written, so the
// step itself is what is checked.
func assertContinuous(t *testing.T, kind string, pkts []packet, frame float64) {
	t.Helper()
	for i := 1; i < len(pkts); i++ {
		prev, cur := pkts[i-1], pkts[i]
		step := cur.pts - prev.pts
		if frame > 0 {
			if step <= 0 || step > frame+0.002 {
				t.Errorf("%s steps %.4fs at %.3fs", kind, step, cur.pts)
				return
			}
			continue
		}
		if gap := cur.pts - (prev.pts + prev.dur); math.Abs(gap) > 0.002 {
			t.Errorf("%s has a %.4fs gap at %.3fs", kind, gap, cur.pts)
			return
		}
	}
}

// A seek to near the end starts ffmpeg there: it does not produce, or even
// read through, the film in between.
func TestASeekFarAheadStartsThereAndSkipsTheGap(t *testing.T) {
	for _, copyVideo := range []bool{true, false} {
		t.Run(fmt.Sprintf("copy=%v", copyVideo), func(t *testing.T) {
			r := newRig(t, 2)
			s := r.open(t, "seek", copyVideo)
			segs := s.Segments()
			// Start where a player resuming at the beginning would.
			initSegment(t, s)
			fetch(t, s, 0)
			// The run makes its readahead and then waits, paused on the pipe,
			// rather than running on through the film.
			await(t, s, func() bool { return s.Position() == hls.DefaultReadahead+1 }, "the run to pause on its readahead")
			if got := s.Cached(); len(got) != hls.DefaultReadahead+1 || got[len(got)-1] != hls.DefaultReadahead {
				t.Fatalf("cached before the seek = %v, want segments 0 to %d", got, hls.DefaultReadahead)
			}
			// Then jump to the last segment.
			last := len(segs) - 1
			b := fetch(t, s, last)

			runs := r.rec.runs()
			final := runs[len(runs)-1].Segmented
			if final == nil || math.Abs(final.From-segs[last].Start) > 1e-9 {
				t.Fatalf("the seek ran from %+v, want the last segment's start %.3f", final, segs[last].Start)
			}
			// The segment it made is the same span a continuous run makes.
			first := ofStream(packets(t, r.ffprobe, initSegment(t, s), b), 0)
			if !first[0].key || math.Abs(first[0].pts-ffmpeg.SegmentTimelineOffset-segs[last].Start) > 0.002 {
				t.Errorf("after the seek, the last segment opens at %.4fs (key=%v), want a keyframe at %.4fs",
					first[0].pts-ffmpeg.SegmentTimelineOffset, first[0].key, segs[last].Start)
			}
			// Two runs in all — the first stopped where it paused — and
			// nothing in between was made.
			if len(runs) != 2 {
				t.Errorf("a far seek took %d runs, want 2", len(runs))
			}
			for _, k := range s.Cached() {
				if k > hls.DefaultReadahead && k < last {
					t.Errorf("segment %d was produced although the player jumped past it", k)
				}
			}
		})
	}
}

// Two clients, two sessions, one slot each; a third client past the cap is
// told it is busy rather than queued. Concurrent requests inside one session
// share its one run.
func TestConcurrentClientsShareTheCap(t *testing.T) {
	r := newRig(t, 2)
	a, b := r.open(t, "a", true), r.open(t, "b", false)

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	// Each client asks for its segments in order, twice over at once — a
	// player retrying, or a HEAD beside a GET.
	for _, s := range []*hls.Session{a, b} {
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := range 3 {
					ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
					f, err := s.Segment(ctx, k)
					cancel()
					if err != nil {
						errs <- err
						return
					}
					_ = f.Close()
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent request failed: %v", err)
	}
	if runs := len(r.rec.runs()); runs != 2 {
		t.Errorf("two sessions with parallel requests took %d runs, want one each", runs)
	}

	// Both slots are held by runs paused on their readahead; a third
	// session is refused, not queued.
	c := r.open(t, "c", true)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := c.Segment(ctx, 0); !errors.Is(err, hls.ErrBusy) {
		t.Errorf("a third client past a cap of two got %v, want ErrBusy", err)
	}
}

// A paused player gives its slot back; a forgotten session takes its cache
// with it.
func TestIdleRunsStopAndIdleSessionsAreCleanedUp(t *testing.T) {
	r := newRig(t, 1)
	s := r.open(t, "idle", true)
	fetch(t, s, 0)
	if r.m.Active() != 1 {
		t.Fatalf("active runs = %d, want 1 while the player is buffering", r.m.Active())
	}

	r.m.Sweep(r.clock.advance(hls.DefaultProducerIdle))
	await(t, s, func() bool { return r.m.Active() == 0 }, "the idle run to stop")
	// The slot is free again: another session can run.
	other := r.open(t, "other", true)
	fetch(t, other, 0)
	r.m.Sweep(r.clock.advance(hls.DefaultProducerIdle))
	await(t, other, func() bool { return r.m.Active() == 0 }, "the second idle run to stop")

	// The paused player resumes: what it buffered is still served, and the
	// first segment past it starts a run again.
	cached := s.Cached()
	fetch(t, s, cached[len(cached)-1])
	fetch(t, s, cached[len(cached)-1]+1)

	r.m.Sweep(r.clock.advance(hls.DefaultSessionIdle))
	for _, id := range []string{"idle", "other"} {
		if left := sessionDirs(t, r.root, id); len(left) != 0 {
			t.Errorf("session %s's cache is still on disk after the session ended: %v", id, left)
		}
	}
	if r.m.Active() != 0 {
		t.Errorf("active runs = %d after every session ended", r.m.Active())
	}
	// A new request for the same id is a new session, not an error.
	fetch(t, r.open(t, "idle", true), 0)
}

// await waits for ok, woken by s's own changes rather than by polling. The
// minute is a guard against a hang, not a deadline the work races.
func await(t *testing.T, s *hls.Session, ok func() bool, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := s.Await(ctx, ok); err != nil {
		t.Fatalf("waiting for %s: %v", what, err)
	}
}

// sessionDirs is every cache directory of the session id: one per time it was
// opened.
func sessionDirs(t *testing.T, root, id string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(root, id+".*"))
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// lingering is a Runner whose runs outlive being stopped until released: an
// ffmpeg slow to exit.
type lingering struct {
	started, release chan struct{}
}

func (l *lingering) Stream(ctx context.Context, _ ffmpeg.StreamSpec, _ io.Writer) error {
	l.started <- struct{}{}
	<-ctx.Done()
	<-l.release
	return ctx.Err()
}

// fixedTiming answers one timing without ffprobe.
type fixedTiming struct{ t hls.Timing }

func (f fixedTiming) Timing(context.Context, string, string, bool) (hls.Timing, error) {
	return f.t, nil
}

func (fixedTiming) Warm(string, string, bool) {}

// A player that resumes just as its idle session is forgotten opens the same
// id again while the old session is still waiting for its run to exit. The old
// session's cleanup must not take the new one's cache with it.
func TestASessionReopenedDuringItsCleanupKeepsItsCache(t *testing.T) {
	root := filepath.Join(t.TempDir(), "hls")
	clk := &clock{now: time.Unix(1_800_000_000, 0)}
	run := &lingering{started: make(chan struct{}, 4), release: make(chan struct{})}
	m, err := hls.New(hls.Options{
		Root: root, Runner: run, Now: clk.Now,
		Index: fixedTiming{t: hls.Timing{Duration: 60, Keyframes: []float64{0, 6, 12, 18, 24, 30, 36, 42, 48, 54}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	src := hls.Source{Blob: "blob", Spec: ffmpeg.StreamSpec{Source: "film.mkv", CopyVideo: true}}

	old, err := m.Open(t.Context(), "resumed", src)
	if err != nil {
		t.Fatal(err)
	}
	initDone := make(chan error, 1)
	go func() {
		_, err := old.Init(t.Context())
		initDone <- err
	}()
	<-run.started

	// The session goes idle and is forgotten; its cleanup waits on the run.
	swept := make(chan struct{})
	go func() {
		m.Sweep(clk.advance(hls.DefaultSessionIdle))
		close(swept)
	}()
	// The waiting request sees the session end only once Sweep has let go of
	// it, so the id is free from here on.
	if err := <-initDone; err == nil {
		t.Fatal("init succeeded on a session that ended")
	}

	again, err := m.Open(t.Context(), "resumed", src)
	if err != nil {
		t.Fatal(err)
	}
	if again == old {
		t.Fatal("an ended session was handed out again")
	}
	if dirs := sessionDirs(t, root, "resumed"); len(dirs) != 2 {
		t.Fatalf("cache directories while the old session is still being removed = %v, want the old one and the new one", dirs)
	}

	close(run.release)
	<-swept
	if dirs := sessionDirs(t, root, "resumed"); len(dirs) != 1 {
		t.Fatalf("cache directories once the old session is removed = %v, want only the new session's", dirs)
	}
	if _, err := os.Stat(sessionDirs(t, root, "resumed")[0]); err != nil {
		t.Errorf("the reopened session lost its cache: %v", err)
	}
}

// What is in the cache directory at start belongs to a previous process.
func TestNewClearsTheCache(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "old-session", "3.m4s")
	if err := os.MkdirAll(filepath.Dir(stale), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hls.New(hls.Options{Root: root, Runner: &recorder{}, Index: &hls.Indexer{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(stale)); !os.IsNotExist(err) {
		t.Errorf("a previous process's session survived New: %v", err)
	}
}

// Every run of one plan writes the same init segment, whichever segment it
// starts at: a player fetches EXT-X-MAP once and decodes every segment with it.
func TestEveryRunWritesTheSameInitSegment(t *testing.T) {
	for _, copyVideo := range []bool{true, false} {
		t.Run(fmt.Sprintf("copy=%v", copyVideo), func(t *testing.T) {
			r := newRig(t, 2)
			s := r.open(t, "init", copyVideo)
			streamer, err := ffmpeg.NewStreamer(ffmpeg.StreamerOptions{FFmpegPath: r.ffmpeg})
			if err != nil {
				t.Fatal(err)
			}
			initOf := func(k int) []byte {
				var out strings.Builder
				if err := streamer.Stream(t.Context(), s.SpecFor(k), &out); err != nil {
					t.Fatalf("run from segment %d: %v", k, err)
				}
				b := []byte(out.String())
				i := strings.Index(out.String(), "moof")
				if i < 4 {
					t.Fatalf("run from segment %d wrote no fragment", k)
				}
				return b[:i-4]
			}
			first, later := initOf(0), initOf(len(s.Segments())-2)
			if string(first) != string(later) {
				t.Errorf("the init segment differs between a run from the start (%d bytes) and one from near the end (%d bytes)",
					len(first), len(later))
			}
		})
	}
}

// A source with no picture has nothing to cut a copied picture at. The
// indexer says it cannot be planned, which the route answers with a 404 that
// sends the player to the live stream, rather than as a fault.
func TestASourceWithNoPictureCannotBePlanned(t *testing.T) {
	ffmpegPath, ffprobePath := toolchain(t)
	src := filepath.Join(t.TempDir(), "song.flac")
	if b, err := exec.CommandContext(t.Context(), ffmpegPath, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "5", src).CombinedOutput(); err != nil {
		t.Fatalf("generating the song: %v\n%s", err, b)
	}
	index, err := hls.NewIndexer(ffprobePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := index.Timing(ctx, "song", src, true); !errors.Is(err, hls.ErrNoPlan) {
		t.Errorf("timing a source with no picture for a copy = %v, want ErrNoPlan", err)
	}
}
