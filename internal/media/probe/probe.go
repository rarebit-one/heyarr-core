// Package probe reads what a media file is, over a seekable Range-capable URL
// rather than by materialising the whole blob (spec §29).
//
// # Why this is not "download it and run ffprobe"
//
// A 20 GB remux is a normal case. Answering "what codec is this" by copying 20
// GB is not, and §29 says so: workers should probe remotely using seekable
// Range-capable URLs whenever possible, with whole-blob materialisation as the
// FALLBACK.
//
// GET /api/v1/blobs/{hash}/content already supports byte ranges, is already
// soak-tested to serve 20 GiB with flat memory, and is deliberately the same
// endpoint replication and the BitTorrent web-seed will use (ADR-0013). None
// of it changes for this. The work is all on the client side of that contract.
//
// # ffprobe is handed a loopback URL, not the real one
//
// The obvious implementation passes the blob URL and a bearer token straight
// to ffprobe with -headers. That was the plan recorded on the issue, and it is
// wrong for two reasons that only became clear while building it:
//
//  1. ffprobe's argv is world-readable in the process table. A bearer token in
//     -headers is a credential on display to every user on the host for the
//     lifetime of the probe.
//  2. §29 asks for whole-blob materialisation as a fallback, which means
//     something has to notice that Range probing is going badly. From outside
//     ffprobe there is nothing to notice WITH: it reports no statistics, and
//     the bytes it pulls are invisible.
//
// So the prober runs a small loopback proxy for the duration of one probe.
// ffprobe is given an unauthenticated 127.0.0.1 URL; the proxy holds the
// credential, adds it upstream, and counts every byte it passes back. That
// makes the measurement real in production rather than only in a test, gives
// the fallback something to trigger on, and keeps the token out of argv.
//
// The cost is one extra loopback hop per probe. For a few hundred kilobytes of
// container header that is not a cost worth optimising, and it buys the two
// things above.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// Result is a domain-neutral description of what a container holds.
//
// It knows nothing about the catalog, the CAS or SQL, so that
// internal/domain/playback can consume it in M2-07 without crossing the
// depguard boundary that keeps the domain storage-agnostic.
type Result struct {
	// Container is ffprobe's format_name — a comma-separated list of every
	// name the demuxer answers to, e.g. "mov,mp4,m4a,3gp,3g2,mj2". It is kept
	// verbatim rather than reduced to one name, because which of those a file
	// "is" is a question with no answer and the planner matches by membership.
	Container   string   `json:"container"`
	FormatLong  string   `json:"format_long,omitempty"`
	DurationSec float64  `json:"duration_seconds,omitempty"`
	BitrateBPS  int64    `json:"bitrate_bps,omitempty"`
	SizeBytes   int64    `json:"size_bytes,omitempty"`
	Streams     []Stream `json:"streams"`
}

// Stream is one elementary stream.
type Stream struct {
	Index      int    `json:"index"`
	Type       string `json:"type"` // video, audio, subtitle, data
	Codec      string `json:"codec"`
	Profile    string `json:"profile,omitempty"`
	Level      int    `json:"level,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	FrameRate  string `json:"frame_rate,omitempty"`
	Channels   int    `json:"channels,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`
	BitrateBPS int64  `json:"bitrate_bps,omitempty"`
	Language   string `json:"language,omitempty"`
}

// VideoStream returns the first video stream, if any.
func (r Result) VideoStream() (Stream, bool) { return r.firstOfType("video") }

// AudioStream returns the first audio stream, if any.
func (r Result) AudioStream() (Stream, bool) { return r.firstOfType("audio") }

func (r Result) firstOfType(kind string) (Stream, bool) {
	for _, s := range r.Streams {
		if s.Type == kind {
			return s, true
		}
	}
	return Stream{}, false
}

// Stats is what the probe cost. It is returned alongside every Result because
// a measurement nobody records is a measurement nobody can act on — and
// because "we said we probe over ranges" is a claim, not evidence.
type Stats struct {
	// BytesRead is what actually crossed the wire from the peer, counted by
	// the proxy. For a well-formed MP4 with a leading moov this is a few
	// hundred kilobytes of a file that may be twenty gigabytes.
	BytesRead int64
	// Requests is how many upstream requests the probe made.
	Requests int
	// Materialised reports that Range probing did not work and the blob was
	// copied whole (§29's fallback). It is counted rather than silent: a
	// fallback nobody can see becomes the default without anyone noticing.
	Materialised bool
	Elapsed      time.Duration
}

// Fraction of the blob read is what BytesRead means relative to its size.
func (s Stats) Fraction(size int64) float64 {
	if size <= 0 {
		return 0
	}
	return float64(s.BytesRead) / float64(size)
}

// Target is one blob to probe.
type Target struct {
	// URL is the peer's blob content endpoint (ADR-0013).
	URL string
	// Token is a scoped, short-lived bearer credential. It is held by the
	// proxy and never appears in ffprobe's argv.
	Token string
	// Size is the blob's size, used for the fallback threshold. Zero disables
	// the threshold — the probe still runs, it just cannot decide it is
	// reading too much.
	Size int64
}

// Defaults.
const (
	// defaultTimeout bounds one probe. Generous, because it covers a fallback
	// that may copy a large blob over a network.
	defaultTimeout = 5 * time.Minute
	// defaultFallbackFraction is how much of a blob a Range probe may read
	// before materialising sequentially is the better trade. Past roughly half,
	// thousands of small ranged reads are strictly worse than one streamed
	// copy — same bytes, far more round trips.
	defaultFallbackFraction = 0.5
	// minFallbackSize is the floor below which the threshold never applies.
	//
	// Without it every small file trips the fallback, because reading 100% of
	// a 30 KB fixture is entirely normal and cheaper than any alternative. The
	// threshold exists to catch a 20 GB file being read in its entirety, not a
	// small one being read efficiently.
	minFallbackSize = 8 << 20
)

// Options configure a Prober.
type Options struct {
	// FFprobePath is the resolved binary (ADR-0023). Required.
	FFprobePath string
	// HTTPClient fetches upstream. Nil means a default with no timeout of its
	// own — the context bounds the probe, and a client timeout would kill a
	// legitimate slow fallback mid-copy.
	HTTPClient *http.Client
	// TempDir is where the fallback materialises. Empty means the OS default.
	TempDir string
	// Timeout bounds one probe. Zero means defaultTimeout.
	Timeout time.Duration
	// FallbackFraction overrides defaultFallbackFraction. Negative disables
	// the threshold entirely, which is the right setting for a test that wants
	// to observe the Range path on a small file.
	FallbackFraction float64
	Logger           *slog.Logger
}

// Prober probes media.
type Prober struct {
	ffprobe  string
	client   *http.Client
	tempDir  string
	timeout  time.Duration
	fraction float64
	log      *slog.Logger
}

// New builds a Prober.
func New(opts Options) (*Prober, error) {
	if opts.FFprobePath == "" {
		return nil, errors.New("probe: an ffprobe path is required")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	fraction := opts.FallbackFraction
	if fraction == 0 {
		fraction = defaultFallbackFraction
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Prober{
		ffprobe: opts.FFprobePath, client: client, tempDir: opts.TempDir,
		timeout: timeout, fraction: fraction, log: log.With("component", "probe"),
	}, nil
}

// validateTarget refuses a URL this package will not fetch.
//
// gosec flags the proxy's upstream request as SSRF, and it is right to: the
// URL decides what gets fetched, and "the caller only ever passes a peer
// endpoint" is a convention rather than a guarantee. A scheme allowlist is the
// guarantee.
//
// http and https only. Everything else — file, gopher, ftp, data — is either a
// way to read the local filesystem through a service that should not, or a
// protocol Range means nothing in. It also removes an entire class of
// confusion: a file:// target silently doing something different from an
// http:// one is the sort of asymmetry that survives for milestones.
func validateTarget(target Target) error {
	if target.URL == "" {
		return errors.New("probe: a target URL is required")
	}
	u, err := url.Parse(target.URL)
	if err != nil {
		return fmt.Errorf("probe: the target URL is unparseable: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("probe: the target must be http or https, not %q — "+
			"a probe fetches a peer's blob endpoint (ADR-0013) and nothing else", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("probe: the target URL names no host")
	}
	return nil
}

// ErrProbeFailed means ffprobe could not describe the target, over ranges or
// whole. It is typed so the job layer can tell "this is not media Heyarr can
// read" from "the network went away", and act differently.
var ErrProbeFailed = errors.New("probe: ffprobe could not read the target")

// Probe describes the blob at target.
//
// It tries the Range path first and falls back to materialising the whole blob
// (§29). The fallback is reported in Stats, never silently.
func (p *Prober) Probe(ctx context.Context, target Target) (Result, Stats, error) {
	if err := validateTarget(target); err != nil {
		return Result{}, Stats{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	started := time.Now()

	result, stats, rangeErr := p.probeOverRange(ctx, target)
	stats.Elapsed = time.Since(started)
	if rangeErr == nil {
		p.log.Debug("probed over ranges",
			"url", target.URL, "bytes_read", stats.BytesRead,
			"requests", stats.Requests, "size", target.Size)
		return result, stats, nil
	}

	// Loud, because this is the expensive path and a fallback nobody can see
	// becomes the default without anyone noticing.
	p.log.Warn("range probing failed; materialising the whole blob (§29 fallback)",
		"url", target.URL, "size", target.Size,
		"bytes_read_before_giving_up", stats.BytesRead, "error", rangeErr)

	result, fallbackStats, err := p.probeWhole(ctx, target)
	fallbackStats.BytesRead += stats.BytesRead
	fallbackStats.Requests += stats.Requests
	fallbackStats.Materialised = true
	fallbackStats.Elapsed = time.Since(started)
	if err != nil {
		return Result{}, fallbackStats, fmt.Errorf("%w: %w", ErrProbeFailed, err)
	}
	return result, fallbackStats, nil
}

// probeOverRange runs ffprobe against a counting loopback proxy.
func (p *Prober) probeOverRange(ctx context.Context, target Target) (Result, Stats, error) {
	px, err := p.startProxy(ctx, target)
	if err != nil {
		return Result{}, Stats{}, err
	}
	defer px.close()

	out, err := p.runFFprobe(ctx, px.url)
	stats := px.stats()
	if err != nil {
		if px.trippedThreshold() {
			return Result{}, stats, fmt.Errorf(
				"range probing read %d of %d bytes (%.0f%%), past the %.0f%% threshold",
				stats.BytesRead, target.Size, 100*stats.Fraction(target.Size), 100*p.fraction)
		}
		return Result{}, stats, err
	}
	result, err := parseAndCheck(out)
	if err != nil {
		return Result{}, stats, err
	}
	if result.SizeBytes == 0 {
		result.SizeBytes = target.Size
	}
	return result, stats, nil
}

// probeWhole is §29's fallback: copy the blob, probe the copy.
func (p *Prober) probeWhole(ctx context.Context, target Target) (Result, Stats, error) {
	var stats Stats

	f, err := os.CreateTemp(p.tempDir, "heyarr-probe-*")
	if err != nil {
		return Result{}, stats, fmt.Errorf("probe: creating the fallback file: %w", err)
	}
	name := f.Name()
	defer func() {
		_ = f.Close()
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			p.log.Warn("the fallback file could not be removed", "path", name, "error", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		return Result{}, stats, err
	}
	if target.Token != "" {
		req.Header.Set("Authorization", "Bearer "+target.Token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return Result{}, stats, fmt.Errorf("probe: fetching the blob: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	stats.Requests++
	if resp.StatusCode != http.StatusOK {
		return Result{}, stats, fmt.Errorf("probe: fetching the blob: %s", resp.Status)
	}

	n, err := io.Copy(f, resp.Body)
	stats.BytesRead += n
	if err != nil {
		return Result{}, stats, fmt.Errorf("probe: copying the blob: %w", err)
	}
	if err := f.Close(); err != nil {
		return Result{}, stats, err
	}

	out, err := p.runFFprobe(ctx, name)
	if err != nil {
		return Result{}, stats, err
	}
	// The same check as the Range path, and not a copy of it.
	//
	// The first version guarded only the Range path, and a test with a
	// stand-in ffprobe caught it: an empty result over ranges correctly fell
	// back, and the FALLBACK then returned that same empty result as a
	// success. A validity rule that applies to one of two paths to the same
	// answer is a rule that will be wrong on whichever path is used less.
	result, err := parseAndCheck(out)
	if err != nil {
		return Result{}, stats, err
	}
	if result.SizeBytes == 0 {
		result.SizeBytes = n
	}
	return result, stats, nil
}

// startProxy brings up the counting loopback proxy for one probe.
func (p *Prober) startProxy(ctx context.Context, target Target) (*proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("probe: opening the loopback proxy: %w", err)
	}
	limit := int64(0)
	if target.Size >= minFallbackSize && p.fraction > 0 {
		limit = int64(float64(target.Size) * p.fraction)
	}
	px := &proxy{
		upstream: target.URL, token: target.Token, client: p.client,
		limit: limit, log: p.log, ctx: ctx,
	}
	px.server = &http.Server{Handler: px, ReadHeaderTimeout: 10 * time.Second}
	px.url = "http://" + ln.Addr().String() + "/blob"
	px.done = make(chan struct{})
	go func() {
		defer close(px.done)
		_ = px.server.Serve(ln)
	}()
	return px, nil
}

// proxy forwards one blob's Range requests upstream, adding the credential and
// counting the bytes it returns.
type proxy struct {
	upstream string
	token    string
	client   *http.Client
	// limit is the byte budget past which the probe is judged to be reading
	// too much. Zero means no limit.
	limit    int64
	log      *slog.Logger
	ctx      context.Context //nolint:containedctx // one proxy exists for exactly one probe and dies with it
	server   *http.Server
	url      string
	done     chan struct{}
	bytes    atomic.Int64
	requests atomic.Int64
	tripped  atomic.Bool
}

func (px *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	px.requests.Add(1)

	req, err := http.NewRequestWithContext(px.ctx, r.Method, px.upstream, nil)
	if err != nil {
		http.Error(w, "bad upstream", http.StatusBadGateway)
		return
	}
	// Only the headers that matter for ranged reads are forwarded. Passing the
	// request through wholesale would forward the loopback Host and whatever
	// else ffprobe sends, to an endpoint that has its own opinions about them.
	for _, h := range []string{"Range", "If-Range", "Accept-Encoding"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if px.token != "" {
		// The credential lives here and only here. ffprobe never sees it, so
		// it never reaches the process table.
		req.Header.Set("Authorization", "Bearer "+px.token)
	}

	// #nosec G704 -- px.upstream is validated by validateTarget before the
	// proxy is started: http or https only, with a host. It is the peer blob
	// endpoint this probe was asked for, and the proxy forwards to that one
	// URL regardless of what path ffprobe requests.
	resp, err := px.client.Do(req)
	if err != nil {
		http.Error(w, "upstream failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	n, err := io.Copy(&countingWriter{w: w, px: px}, resp.Body)
	px.bytes.Add(n)
	if err != nil && !errors.Is(err, errBudgetExceeded) {
		px.log.Debug("the proxy stopped copying", "error", err)
	}
}

// errBudgetExceeded stops the copy when the probe has read too much.
var errBudgetExceeded = errors.New("probe: range budget exceeded")

// countingWriter tallies bytes and enforces the budget.
//
// Enforcing here rather than after the fact is what makes the threshold a
// TRIGGER rather than a report: a probe that would read the whole blob in
// small ranges is stopped partway, and the fallback — one sequential copy of
// the same bytes — is strictly the better trade at that point.
type countingWriter struct {
	w  io.Writer
	px *proxy
	n  int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.px.limit > 0 {
		if total := c.px.bytes.Load() + c.n + int64(len(p)); total > c.px.limit {
			c.px.tripped.Store(true)
			return 0, errBudgetExceeded
		}
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (px *proxy) stats() Stats {
	return Stats{BytesRead: px.bytes.Load(), Requests: int(px.requests.Load())}
}

func (px *proxy) trippedThreshold() bool { return px.tripped.Load() }

func (px *proxy) close() {
	_ = px.server.Close()
	<-px.done
}

// runFFprobe asks ffprobe to describe a target — a URL or a path — as JSON.
func (p *Prober) runFFprobe(ctx context.Context, target string) ([]byte, error) {
	out, err := runCommand(ctx, p.ffprobe,
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		target)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProbeFailed, err)
	}
	return out, nil
}

// ffprobeJSON is the shape ffprobe emits. Every numeric field arrives as a
// STRING, which is why they are parsed rather than typed: ffprobe's JSON
// writer quotes everything, and a struct that declared them as numbers would
// fail to unmarshal against the real tool while passing against a hand-written
// fixture.
type ffprobeJSON struct {
	Format struct {
		FormatName     string `json:"format_name"`
		FormatLongName string `json:"format_long_name"`
		Duration       string `json:"duration"`
		Size           string `json:"size"`
		BitRate        string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		Index          int               `json:"index"`
		CodecName      string            `json:"codec_name"`
		CodecType      string            `json:"codec_type"`
		Profile        string            `json:"profile"`
		Level          int               `json:"level"`
		Width          int               `json:"width"`
		Height         int               `json:"height"`
		RFrameRate     string            `json:"r_frame_rate"`
		Channels       int               `json:"channels"`
		SampleRate     string            `json:"sample_rate"`
		BitRate        string            `json:"bit_rate"`
		Tags           map[string]string `json:"tags"`
		Disposition    map[string]int    `json:"disposition"`
		CodecTagString string            `json:"codec_tag_string"`
	} `json:"streams"`
}

// parseAndCheck decodes ffprobe's output and rejects a result that cannot
// describe playable media.
//
// A container ffprobe opened but found nothing in is not a success: handing
// the playback planner a Result with no streams means asking it to plan a
// playback of nothing. Every path that produces a Result goes through here.
func parseAndCheck(raw []byte) (Result, error) {
	result, err := parse(raw)
	if err != nil {
		return Result{}, err
	}
	if len(result.Streams) == 0 {
		return Result{}, errors.New("probe: the target declares no streams")
	}
	return result, nil
}

func parse(raw []byte) (Result, error) {
	var doc ffprobeJSON
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Result{}, fmt.Errorf("probe: ffprobe returned something that is not JSON: %w", err)
	}
	out := Result{
		Container:   doc.Format.FormatName,
		FormatLong:  doc.Format.FormatLongName,
		DurationSec: parseFloat(doc.Format.Duration),
		BitrateBPS:  parseInt(doc.Format.BitRate),
		SizeBytes:   parseInt(doc.Format.Size),
		Streams:     make([]Stream, 0, len(doc.Streams)),
	}
	for _, s := range doc.Streams {
		out.Streams = append(out.Streams, Stream{
			Index:      s.Index,
			Type:       s.CodecType,
			Codec:      s.CodecName,
			Profile:    s.Profile,
			Level:      s.Level,
			Width:      s.Width,
			Height:     s.Height,
			FrameRate:  s.RFrameRate,
			Channels:   s.Channels,
			SampleRate: int(parseInt(s.SampleRate)),
			BitrateBPS: parseInt(s.BitRate),
			Language:   s.Tags["language"],
		})
	}
	return out, nil
}

// parseFloat and parseInt tolerate ffprobe's "N/A" and empty values, which it
// emits for anything a container did not declare. Those are absences, not
// errors: a Matroska file legitimately has no overall bit rate.
func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func parseInt(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}
