package hls

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Timing is what a segment plan needs from a source: where its clock starts,
// how long it runs, and — for a picture that will be copied — where its
// keyframes are.
type Timing struct {
	// Start is the source's start time (ffprobe's format start_time), the
	// zero a re-encode's segment timeline counts from.
	Start float64
	// Duration is the source's running time.
	Duration float64
	// Keyframes are the video keyframes' presentation times, ascending, on
	// the source's own clock. Nil unless asked for.
	Keyframes []float64
}

// Indexer reads a source's Timing with ffprobe, once per blob.
//
// # Why a whole pass, and why it is remembered
//
// A copied picture can only be cut where the source has keyframes, and the
// playlist has to list every segment before the first is fetched, so every
// keyframe in the film is needed up front. ffprobe reads them off the packet
// flags — a demux, no decode — which reads the whole file once. That is
// seconds for a film on a fast disk and can be minutes for a large remux on a
// spinning one, so the answer is kept for the process's life (it is a fact
// about immutable bytes, keyed by their digest), computed once however many
// clients ask at the same moment, and started at planning time so the
// playlist request usually finds it done.
type Indexer struct {
	bin string
	log *slog.Logger
	// timeout bounds one pass. It is detached from the request that started
	// it: a television that gives up waiting has not made the answer worth
	// less to the retry it is about to make.
	timeout time.Duration

	mu       sync.Mutex
	done     map[indexKey]Timing
	order    []indexKey
	inflight map[indexKey]*indexCall
}

type indexKey struct {
	blob      string
	keyframes bool
}

type indexCall struct {
	done chan struct{}
	t    Timing
	err  error
}

// indexMemory is how many blobs' timings are kept. A keyframe list for a
// two-hour film with two-second GOPs is a few thousand floats.
const indexMemory = 64

// DefaultIndexTimeout bounds one ffprobe pass over a source.
const DefaultIndexTimeout = 15 * time.Minute

// NewIndexer builds an Indexer over the resolved ffprobe (ADR-0023).
func NewIndexer(ffprobePath string, log *slog.Logger) (*Indexer, error) {
	if ffprobePath == "" {
		return nil, errors.New("hls: an ffprobe path is required")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Indexer{
		bin: ffprobePath, log: log.With("component", "hls.index"), timeout: DefaultIndexTimeout,
		done: map[indexKey]Timing{}, inflight: map[indexKey]*indexCall{},
	}, nil
}

// Timing returns the source's timing, reading it if nothing has yet. blob is
// the source's digest (invariant 1): the path is where to read, the digest is
// what the answer is about.
func (x *Indexer) Timing(ctx context.Context, blob, path string, keyframes bool) (Timing, error) {
	call := x.start(blob, path, keyframes)
	select {
	case <-call.done:
		return call.t, call.err
	case <-ctx.Done():
		return Timing{}, ctx.Err()
	}
}

// Warm starts reading a source's timing without waiting for it.
func (x *Indexer) Warm(blob, path string, keyframes bool) { x.start(blob, path, keyframes) }

func (x *Indexer) start(blob, path string, keyframes bool) *indexCall {
	key := indexKey{blob: blob, keyframes: keyframes}
	x.mu.Lock()
	defer x.mu.Unlock()
	if t, ok := x.done[key]; ok {
		call := &indexCall{done: make(chan struct{}), t: t}
		close(call.done)
		return call
	}
	if call, ok := x.inflight[key]; ok {
		return call
	}
	call := &indexCall{done: make(chan struct{})}
	x.inflight[key] = call
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), x.timeout)
		defer cancel()
		started := time.Now()
		call.t, call.err = x.read(ctx, path, keyframes)
		x.mu.Lock()
		delete(x.inflight, key)
		if call.err == nil {
			x.remember(key, call.t)
		}
		x.mu.Unlock()
		close(call.done)
		if call.err != nil {
			x.log.Warn("reading a source's timing failed", "blob", blob, "error", call.err)
			return
		}
		x.log.Info("source timing read", "blob", blob, "keyframes", len(call.t.Keyframes),
			"duration", call.t.Duration, "elapsed", time.Since(started))
	}()
	return call
}

func (x *Indexer) remember(key indexKey, t Timing) {
	x.done[key] = t
	x.order = append(x.order, key)
	for len(x.order) > indexMemory {
		delete(x.done, x.order[0])
		x.order = x.order[1:]
	}
}

// IndexArgs is the ffprobe command line. With keyframes it lists every packet
// of the first real video stream with its presentation time and flags; the
// format section comes either way. Compact output names each line's section,
// so the two kinds of line cannot be confused.
func IndexArgs(path string, keyframes bool) []string {
	args := []string{"-v", "error", "-of", "compact=p=1:nk=0"}
	if keyframes {
		args = append(args, "-select_streams", "V:0", "-show_entries", "packet=pts_time,flags:format=start_time,duration")
	} else {
		args = append(args, "-show_entries", "format=start_time,duration")
	}
	return append(args, path)
}

func (x *Indexer) read(ctx context.Context, path string, keyframes bool) (Timing, error) {
	// #nosec G204 -- the binary is the resolved toolchain (ADR-0023) and the
	// path is one the CAS handed back.
	cmd := exec.CommandContext(ctx, x.bin, IndexArgs(path, keyframes)...)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Timing{}, err
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4 << 10}
	if err := cmd.Start(); err != nil {
		return Timing{}, err
	}
	t, parseErr := ParseIndex(out)
	if parseErr != nil {
		_, _ = io.Copy(io.Discard, out)
	}
	if err := cmd.Wait(); err != nil {
		return Timing{}, fmt.Errorf("hls: ffprobe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if parseErr != nil {
		return Timing{}, parseErr
	}
	if keyframes && len(t.Keyframes) == 0 {
		return Timing{}, errors.New("hls: the source lists no video keyframes")
	}
	return t, nil
}

// ParseIndex reads IndexArgs' output.
func ParseIndex(r io.Reader) (Timing, error) {
	var t Timing
	haveDuration := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		section, fields, ok := strings.Cut(sc.Text(), "|")
		if !ok {
			continue
		}
		kv := map[string]string{}
		for _, f := range strings.Split(fields, "|") {
			if k, v, ok := strings.Cut(f, "="); ok {
				kv[k] = v
			}
		}
		switch section {
		case "packet":
			if !strings.Contains(kv["flags"], "K") {
				continue
			}
			if pts, err := strconv.ParseFloat(kv["pts_time"], 64); err == nil {
				t.Keyframes = append(t.Keyframes, pts)
			}
		case "format":
			if v, err := strconv.ParseFloat(kv["start_time"], 64); err == nil {
				t.Start = v
			}
			if v, err := strconv.ParseFloat(kv["duration"], 64); err == nil && v > 0 {
				t.Duration, haveDuration = v, true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Timing{}, err
	}
	if !haveDuration {
		return Timing{}, errors.New("hls: the source has no duration, so it has no playlist")
	}
	// Packets come in decode order; a keyframe's presentation is in order
	// with the others, but sorting costs nothing and a stray duplicate (an
	// open-GOP stream can flag two) must not become a zero-length segment.
	slices.Sort(t.Keyframes)
	t.Keyframes = slices.Compact(t.Keyframes)
	return t, nil
}

// limitedWriter keeps the first n bytes; ffprobe's stderr is for the log.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		q := p
		if len(q) > l.n {
			q = q[:l.n]
		}
		_, _ = l.w.Write(q)
		l.n -= len(q)
	}
	return len(p), nil
}
