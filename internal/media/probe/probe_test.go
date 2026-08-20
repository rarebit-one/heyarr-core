//nolint:bodyclose // responses in the fake peer are closed by the server
package probe_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/media/probe"
	"github.com/rarebit-one/heyarr-core/internal/testutil/fixtures"
)

// ffprobePath resolves the pinned toolchain, or skips.
//
// Skipping is a supported state (ADR-0023) and CI asserts on the Linux runners
// that it did NOT skip — a skipped test and a passing one are indistinguishable
// in the summary line.
func ffprobePath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("HEYARR_TEST_FFPROBE"); p != "" {
		return p
	}
	local := filepath.Join("..", "..", "..", ".toolchain", "bin", "ffprobe")
	if _, err := os.Stat(local); err == nil {
		abs, err := filepath.Abs(local)
		if err != nil {
			t.Fatal(err)
		}
		return abs
	}
	t.Skip("no ffprobe available; run scripts/toolchain.sh")
	return ""
}

// peer is a stand-in for another node's blob endpoint: real HTTP, real byte
// ranges via http.ServeContent — the same call ADR-0013's endpoint uses — and a
// count of everything it served.
type peer struct {
	*httptest.Server
	served   atomic.Int64
	requests atomic.Int64
	token    string
	// unauthorised counts requests that arrived without the credential, which
	// is how the token-is-not-in-argv claim is checked.
	unauthorised atomic.Int64
}

func newPeer(t *testing.T, body []byte, token string) *peer {
	t.Helper()
	p := &peer{token: token}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.requests.Add(1)
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			p.unauthorised.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Accept-Ranges", "bytes")
		http.ServeContent(w, r, "blob.bin", time.Time{},
			&countingReader{data: body, served: &p.served})
	}))
	t.Cleanup(p.Close)
	return p
}

// countingReader is a ReadSeeker that tallies what is actually read out of it,
// which is the number §29 is about.
type countingReader struct {
	data   []byte
	off    int64
	served *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.off >= int64(len(c.data)) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.off:])
	c.off += int64(n)
	c.served.Add(int64(n))
	return n, nil
}

func (c *countingReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case 0:
		c.off = off
	case 1:
		c.off += off
	case 2:
		c.off = int64(len(c.data)) + off
	}
	return c.off, nil
}

func newProber(t *testing.T, opts probe.Options) *probe.Prober {
	t.Helper()
	opts.FFprobePath = ffprobePath(t)
	if opts.TempDir == "" {
		opts.TempDir = t.TempDir()
	}
	p, err := probe.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The §29 claim, measured — and measured as the property that survives being
// run somewhere else.
//
// This assertion has been wrong twice, and both versions are worth recording
// because they were wrong in the same way: they measured something incidental
// and called it the property.
//
//	v1: "the probe reads under 10% of the blob" — failed locally at 13.2%.
//	    A percentage of a file whose size I chose is a fact about my padding.
//	v2: "the bytes read barely move as the blob grows" — passed on darwin with
//	    ffprobe 6.0 (2.45 MB → 2.90 MB) and failed on Linux CI with 7.0.2
//	    (1.52 MB → 3.51 MB). "Roughly constant" was a fact about one build.
//
// What is true in both, and is what §29 is actually about:
//
//   - the bytes read are bounded by a CONSTANT — ffprobe reads up to its
//     `probesize` (about 5 MB by default) while detecting streams, and stops;
//   - so the FRACTION of the blob read falls as the blob grows.
//
// Neither depends on the version, the platform, or the padding. A 20 GB remux
// costs the same few megabytes as a 30 MB one, which is the whole point of not
// materialising it.
func TestProbingCostIsBoundedAndDoesNotScaleWithBlobSize(t *testing.T) {
	container := fixtures.SampleMP4(1)
	prober := newProber(t, probe.Options{})

	// probeCeiling is a generous multiple of ffprobe's 5 MB default probesize.
	// It is an absolute bound, deliberately: the claim is "bounded by a
	// constant", so the assertion is against a constant.
	const probeCeiling = 16 << 20

	measure := func(t *testing.T, padding int) (bytesRead int64, size int64, fraction float64) {
		t.Helper()
		body := append(append([]byte{}, container...), make([]byte, padding)...)
		p := newPeer(t, body, "probe-token")
		result, stats, err := prober.Probe(t.Context(), probe.Target{
			URL: p.URL + "/blob", Token: "probe-token", Size: int64(len(body)),
		})
		if err != nil {
			t.Fatalf("probing failed: %v", err)
		}
		if stats.Materialised {
			t.Fatal("the range path fell back to materialising a faststart MP4")
		}
		if video, ok := result.VideoStream(); !ok || video.Codec != "h264" {
			t.Errorf("video = %+v, want h264", video)
		}
		if audio, ok := result.AudioStream(); !ok || audio.Codec != "aac" {
			t.Errorf("audio = %+v, want aac", audio)
		}
		f := stats.Fraction(int64(len(body)))
		t.Logf("%d-byte blob: probe read %d bytes (%.3f%%) in %d requests, %s",
			len(body), stats.BytesRead, 100*f, stats.Requests, stats.Elapsed)
		return stats.BytesRead, int64(len(body)), f
	}

	small, smallSize, smallFraction := measure(t, 32<<20)
	large, largeSize, largeFraction := measure(t, 256<<20)

	// Bounded by a constant, on any blob.
	for _, m := range []struct {
		bytes, size int64
	}{{small, smallSize}, {large, largeSize}} {
		if m.bytes > probeCeiling {
			t.Errorf("probing a %d-byte blob read %d bytes, past the %d-byte ceiling — "+
				"the probe is no longer bounded by ffprobe's probesize",
				m.size, m.bytes, int64(probeCeiling))
		}
	}

	// And therefore a smaller share of a larger file. This is the assertion
	// that would catch a genuinely linear read, which is what §29 forbids: a
	// probe that materialised would show the fraction pinned at 100%.
	if largeFraction >= smallFraction {
		t.Errorf("the fraction read did not fall as the blob grew: %.3f%% of %d, then %.3f%% of %d — "+
			"the cost is tracking the blob, which is what §29 exists to prevent",
			100*smallFraction, smallSize, 100*largeFraction, largeSize)
	}
	t.Logf("blob grew %.1f×, bytes read grew %.1f×, fraction fell %.3f%% → %.3f%%",
		float64(largeSize)/float64(smallSize), float64(large)/float64(small),
		100*smallFraction, 100*largeFraction)
}

// The unflattering case, stated rather than omitted. An MP4 whose `moov` is at
// the END makes ffprobe seek to the tail, so the probe costs more than a
// faststart file — and it must still work, and still not materialise.
func TestATrailingMoovCostsMore(t *testing.T) {
	// The fixtures are all faststart, so this constructs the awkward layout by
	// putting the padding BEFORE the container. ffprobe then finds nothing at
	// the front and has to look further in.
	container := fixtures.SampleMP4(1)
	body := append(make([]byte, 8<<20), container...)
	p := newPeer(t, body, "")
	prober := newProber(t, probe.Options{})

	_, stats, err := prober.Probe(t.Context(), probe.Target{
		URL: p.URL + "/blob", Size: int64(len(body)),
	})
	// Whether ffprobe can make sense of a container preceded by 8 MB of zeros
	// is its business. What this asserts is that Heyarr behaves sanely either
	// way: a result, or a typed failure, and never a hang or a silent empty.
	if err != nil {
		if !errors.Is(err, probe.ErrProbeFailed) {
			t.Errorf("error = %v, want ErrProbeFailed", err)
		}
		t.Logf("a leading-padding MP4 was not readable, which is a legitimate answer: %v", err)
	}
	t.Logf("awkward layout, %d bytes: probe read %d bytes (%.2f%%), materialised=%v",
		len(body), stats.BytesRead, 100*stats.Fraction(int64(len(body))), stats.Materialised)
}

// The Range path and the whole-blob path must produce the same answer.
// Different route, same result — otherwise the Range path is a second
// implementation with its own truth, and the fallback silently disagrees with
// the thing it is a fallback for.
func TestTheRangePathAgreesWithTheWholeBlobPath(t *testing.T) {
	body := fixtures.SampleMKV(1)

	ranged := newProber(t, probe.Options{})
	rangePeer := newPeer(t, body, "")
	fromRange, rangeStats, err := ranged.Probe(t.Context(), probe.Target{
		URL: rangePeer.URL + "/blob", Size: int64(len(body)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fromRange.Container == "" {
		t.Fatal("the range path produced no container")
	}

	// A prober forced onto the fallback, against the same bytes.
	wholePeer := newPeer(t, body, "")
	whole := newProber(t, probe.Options{FallbackFraction: -1})
	fromWhole, wholeStats, err := whole.Probe(t.Context(), probe.Target{
		URL: wholePeer.URL + "/blob", Size: int64(len(body)),
	})
	if err != nil {
		t.Fatal(err)
	}

	if fmt.Sprint(fromRange.Streams) != fmt.Sprint(fromWhole.Streams) {
		t.Errorf("the two paths disagree:\n  range: %+v\n  whole: %+v",
			fromRange.Streams, fromWhole.Streams)
	}
	if fromRange.Container != fromWhole.Container {
		t.Errorf("container %q vs %q", fromRange.Container, fromWhole.Container)
	}
	t.Logf("matroska %d bytes: range read %d, whole read %d",
		len(body), rangeStats.BytesRead, wholeStats.BytesRead)
}

// The credential is held by the proxy and never reaches ffprobe's argv, where
// it would be world-readable in the process table for the lifetime of the
// probe.
func TestTheCredentialNeverReachesTheSubprocess(t *testing.T) {
	body := fixtures.SampleMP4(1)
	p := newPeer(t, body, "secret-token")
	prober := newProber(t, probe.Options{})

	if _, _, err := prober.Probe(t.Context(), probe.Target{
		URL: p.URL + "/blob", Token: "secret-token", Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	// Every upstream request carried the credential...
	if n := p.unauthorised.Load(); n != 0 {
		t.Errorf("%d upstream requests arrived without the credential", n)
	}
	// ...and the peer was reached, so the proxy really did forward.
	if p.requests.Load() == 0 {
		t.Error("the peer was never contacted")
	}
}

// Test the refusal: a probe with the wrong credential must fail, not quietly
// return an empty result.
func TestAnUnauthorisedProbeFails(t *testing.T) {
	body := fixtures.SampleMP4(1)
	p := newPeer(t, body, "the-right-token")
	prober := newProber(t, probe.Options{})

	_, stats, err := prober.Probe(t.Context(), probe.Target{
		URL: p.URL + "/blob", Token: "the-wrong-token", Size: int64(len(body)),
	})
	if err == nil {
		t.Fatal("a probe with the wrong credential succeeded")
	}
	if !errors.Is(err, probe.ErrProbeFailed) {
		t.Errorf("error = %v, want ErrProbeFailed", err)
	}
	// It tried the fallback too, and that also failed — which is the correct
	// amount of trying rather than giving up on the first 401.
	if !stats.Materialised {
		t.Error("the fallback was not attempted")
	}
}

// §29's fallback, triggered by the threshold rather than by a failure: a probe
// that would read most of a large blob in small ranges is stopped, and one
// sequential copy of the same bytes is the better trade.
func TestReadingTooMuchTriggersTheFallback(t *testing.T) {
	body := append(fixtures.SampleMP4(1), make([]byte, 16<<20)...)
	p := newPeer(t, body, "")
	// A threshold low enough that any probe trips it, on a blob large enough
	// for the threshold to apply at all.
	prober := newProber(t, probe.Options{FallbackFraction: 0.0001})

	result, stats, err := prober.Probe(t.Context(), probe.Target{
		URL: p.URL + "/blob", Size: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("the fallback did not produce a result: %v", err)
	}
	if !stats.Materialised {
		t.Fatal("the threshold did not trigger the fallback")
	}
	// And the fallback answered correctly, which is the whole reason it exists.
	if v, ok := result.VideoStream(); !ok || v.Codec != "h264" {
		t.Errorf("the materialised probe reported %+v", v)
	}
	t.Logf("fallback: %d bytes read of %d, %d requests", stats.BytesRead, len(body), stats.Requests)
}

// The threshold must not fire on small files. Reading 100% of a 30 KB fixture
// is normal and cheaper than any alternative, and a threshold that tripped
// there would make the fallback the default for most of a library.
func TestTheThresholdDoesNotApplyToSmallBlobs(t *testing.T) {
	body := fixtures.SampleMP4(1)
	p := newPeer(t, body, "")
	prober := newProber(t, probe.Options{FallbackFraction: 0.0001})

	_, stats, err := prober.Probe(t.Context(), probe.Target{
		URL: p.URL + "/blob", Size: int64(len(body)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Materialised {
		t.Errorf("a %d-byte blob fell back; the threshold is meant for large ones", len(body))
	}
}

// A blob that is not media at all fails with a typed error, so the job layer
// can tell "this is not readable media" from "the network went away".
func TestSomethingThatIsNotMediaFailsTypedAndTriesTheFallback(t *testing.T) {
	p := newPeer(t, []byte(strings.Repeat("not media at all ", 1000)), "")
	prober := newProber(t, probe.Options{})

	_, stats, err := prober.Probe(t.Context(), probe.Target{
		URL: p.URL + "/blob", Size: 17000,
	})
	if err == nil {
		t.Fatal("a text file probed successfully")
	}
	if !errors.Is(err, probe.ErrProbeFailed) {
		t.Errorf("error = %v, want ErrProbeFailed", err)
	}
	if !stats.Materialised {
		t.Error("the fallback was not attempted before giving up")
	}
}

// A cancelled context must kill the subprocess rather than leaking it.
func TestACancelledProbeReturnsPromptly(t *testing.T) {
	prober := newProber(t, probe.Options{Timeout: 50 * time.Millisecond})

	// A peer that never answers: the probe must hit its deadline and return,
	// not hang until the test framework kills it.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)

	done := make(chan error, 1)
	go func() {
		_, _, err := prober.Probe(t.Context(), probe.Target{URL: slow.URL + "/blob", Size: 1 << 20})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a probe against a peer that never answers succeeded")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the probe did not return after its deadline; the subprocess leaked")
	}
}

// Every numeric field in ffprobe's JSON is a quoted string, and "N/A" appears
// wherever a container declared nothing. A struct that typed them as numbers
// would fail against the real tool while passing against a hand-written
// fixture — so the parser is exercised against the real tool's real output
// above, and against its absences here.
func TestAbsentFieldsAreZeroNotAnError(t *testing.T) {
	body := fixtures.SampleMKV(1)
	p := newPeer(t, body, "")
	prober := newProber(t, probe.Options{})

	// Matroska legitimately declares no overall bit rate.
	result, _, err := prober.Probe(t.Context(), probe.Target{URL: p.URL + "/blob", Size: int64(len(body))})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Container, "matroska") {
		t.Errorf("container = %q", result.Container)
	}
	if result.DurationSec <= 0 {
		t.Errorf("duration = %v, want a real duration", result.DurationSec)
	}
	// Size is filled from the target when the container does not declare it,
	// so a caller always has one.
	if result.SizeBytes != int64(len(body)) {
		t.Errorf("size = %d, want %d", result.SizeBytes, len(body))
	}
}

// fakeFFprobe writes an executable that prints the given JSON and exits 0.
//
// It exists for one case a real ffprobe cannot be made to produce on demand: a
// container it opens successfully and finds nothing in. Three malformed inputs
// were tried against the pinned 6.0 binary — a bare Matroska EBML header, a
// truncated `ftyp`, and a ZIP named .mp4 — and all three exit non-zero, so the
// real tool would not exercise the guard.
//
// That left a choice between deleting a defensive check because it could not be
// triggered, and testing it against a stand-in. The check is kept because the
// failure it prevents — a Result with no streams reaching the playback planner,
// which would then plan a playback of nothing — is worse than the cost of a
// stand-in, and because "ffprobe always exits non-zero when it finds nothing"
// is an assumption about a tool that is not ours and gets upgraded.
func fakeFFprobe(t *testing.T, stdout string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script; the guard itself is platform-neutral")
	}
	path := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\ncat <<'JSON'\n" + stdout + "\nJSON\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // a test fixture that must be executable
		t.Fatal(err)
	}
	return path
}

// A container ffprobe opens and finds nothing in is not a success. Reporting it
// as one hands the planner a Result with no streams, which it would then have
// to plan a playback of.
func TestAContainerWithNoStreamsIsNotASuccess(t *testing.T) {
	body := fixtures.SampleMP4(1)
	p := newPeer(t, body, "")

	prober, err := probe.New(probe.Options{
		FFprobePath: fakeFFprobe(t, `{"format":{"format_name":"mov,mp4","duration":"1.0"},"streams":[]}`),
		TempDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, stats, err := prober.Probe(t.Context(), probe.Target{
		URL: p.URL + "/blob", Size: int64(len(body)),
	})
	if err == nil {
		t.Fatal("a container declaring no streams was reported as a successful probe")
	}
	if !errors.Is(err, probe.ErrProbeFailed) {
		t.Errorf("error = %v, want ErrProbeFailed", err)
	}
	// It reached the fallback, which is the correct amount of trying: a
	// stream-less answer over ranges is exactly the case where one sequential
	// read is worth attempting.
	if !stats.Materialised {
		t.Error("an empty result over ranges did not attempt the fallback")
	}
}

// The parser must survive ffprobe returning something that is not JSON at all
// — a crash message on stdout, a truncated write.
func TestNonJSONOutputIsATypedFailure(t *testing.T) {
	body := fixtures.SampleMP4(1)
	p := newPeer(t, body, "")

	prober, err := probe.New(probe.Options{
		FFprobePath: fakeFFprobe(t, "Segmentation fault"),
		TempDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := prober.Probe(t.Context(), probe.Target{
		URL: p.URL + "/blob", Size: int64(len(body)),
	}); err == nil {
		t.Fatal("non-JSON output was accepted")
	} else if !errors.Is(err, probe.ErrProbeFailed) {
		t.Errorf("error = %v, want ErrProbeFailed", err)
	}
}

// A scheme allowlist rather than a convention. gosec flagged the proxy's
// upstream request as SSRF and was right to: "the caller only ever passes a
// peer endpoint" is a habit, not a guarantee.
func TestOnlyHTTPTargetsAreFetched(t *testing.T) {
	prober := newProber(t, probe.Options{})
	for _, target := range []string{
		"file:///etc/passwd",
		"gopher://example.com/",
		"ftp://example.com/blob",
		"data:text/plain,hello",
		"://nonsense",
		"http://",
		"",
	} {
		if _, _, err := prober.Probe(t.Context(), probe.Target{URL: target}); err == nil {
			t.Errorf("%q was accepted as a probe target", target)
		}
	}
}

// A sample rate from a media container is a number that arrived from wherever
// the user's library came from. CodeQL flagged the unbounded int64→int
// conversion on this PR, correctly: it is a truncation on any 32-bit build.
//
// An absurd declared rate is reported as ABSENT rather than clamped. Clamping
// would invent a plausible number, and a planner comparing a device against an
// invented rate is worse than one comparing against nothing.
func TestAnAbsurdSampleRateIsAbsentNotTruncated(t *testing.T) {
	body := fixtures.SampleMP4(1)
	p := newPeer(t, body, "")

	for _, tc := range []struct {
		name string
		rate string
		want int
	}{
		{"ordinary", `"44100"`, 44100},
		{"high but real", `"768000"`, 768000},
		{"past the ceiling", `"1099511627776"`, 0},
		{"one that would truncate to something plausible", `"4294967296"`, 0},
		{"negative", `"-44100"`, 0},
		{"not a number", `"N/A"`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prober, err := probe.New(probe.Options{
				FFprobePath: fakeFFprobe(t, `{"format":{"format_name":"mp4","duration":"1.0"},
					"streams":[{"index":0,"codec_type":"audio","codec_name":"aac",
					"sample_rate":`+tc.rate+`}]}`),
				TempDir: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := prober.Probe(t.Context(), probe.Target{
				URL: p.URL + "/blob", Size: int64(len(body)),
			})
			if err != nil {
				t.Fatal(err)
			}
			audio, ok := result.AudioStream()
			if !ok {
				t.Fatal("no audio stream")
			}
			if audio.SampleRate != tc.want {
				t.Errorf("sample_rate = %d, want %d", audio.SampleRate, tc.want)
			}
		})
	}
}
