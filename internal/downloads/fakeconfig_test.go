package downloads

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

func resolvedFake(t *testing.T, caps ...providers.Capability) providers.Resolved {
	t.Helper()
	return providers.Resolved{
		Name: "acceptance-downloads", Kind: providers.KindFake, Capabilities: caps,
		PathMap: []providers.PathMapping{{Remote: "/downloads/complete", Local: t.TempDir()}},
	}
}

// THE assertion #247 exists for: a fake download client can be built from
// configuration, so the demo can reach it.
//
// downloads.Fake is production code, and its doc says why: "putting it in a
// _test.go file would mean the demo could not reach it". Until this, the demo
// could not reach it anyway — only the transmission kind had a constructor and
// that needs a daemon.
func TestAFakeDownloadClientCanBeBuiltFromConfiguration(t *testing.T) {
	p, handled, err := Constructor(resolvedFake(t, providers.CapabilityDownload), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("the constructor did not claim a fake download client, so the registry " +
			"reports it as configured-but-not-implemented")
	}
	if _, ok := p.(providers.Downloader); !ok {
		t.Fatalf("built %T, which is not a Downloader", p)
	}
}

// A fake that ALSO indexes is left to providers.Fake.
//
// That one answers Search from configured offers. Taking it here would build a
// download client with no Search, and the want would stop finding anything —
// a failure that looks like a broken indexer rather than a claimed provider.
func TestAFakeThatAlsoIndexesIsNotClaimedHere(t *testing.T) {
	for _, caps := range [][]providers.Capability{
		{providers.CapabilityIndexer},
		{providers.CapabilityIndexer, providers.CapabilityDownload},
	} {
		_, handled, err := Constructor(resolvedFake(t, caps...), nil)
		if err != nil {
			t.Fatal(err)
		}
		if handled {
			t.Errorf("claimed a fake with capabilities %v, removing its Search", caps)
		}
	}
}

// A fake with nowhere to write is refused rather than defaulted.
//
// A temp directory nobody named would produce completed files the scanner never
// walks and an ingest that cannot find them — reported as a transfer that
// finished and vanished, which is a far harder thing to debug than a refusal at
// startup.
func TestAFakeDownloadClientWithNoPathMapIsRefused(t *testing.T) {
	r := resolvedFake(t, providers.CapabilityDownload)
	r.PathMap = nil

	_, handled, err := Constructor(r, nil)
	if !handled {
		t.Fatal("an unusable fake was left unclaimed rather than refused")
	}
	if err == nil {
		t.Fatal("a fake with nowhere to write was accepted")
	}
	if !strings.Contains(err.Error(), "path_map") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}
}

// A simulated fake invents its own content and finishes in exactly two
// observations — bytes moving, then done.
//
// One step per OBSERVATION rather than per second: a demo that waited on
// wall-clock time flakes on a loaded machine, and the poll beat already drives
// §64. Two passes, every run, on any machine.
func TestASimulatedFakeProgressesOncePerObservation(t *testing.T) {
	f := NewFake("sim", t.TempDir()).Simulate(4096)

	added, err := f.Add(t.Context(), secret.Value("magnet:?xt=urn:btih:abc"))
	if err != nil {
		t.Fatal(err)
	}
	if added.Done {
		t.Fatal("a transfer was finished before anything observed it, so DOWNLOADING " +
			"is never seen and the pipeline's middle is never exercised")
	}

	first, err := f.Transfers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("%d transfers, want 1", len(first))
	}
	if first[0].Done || first[0].BytesDone == 0 {
		t.Errorf("first observation: done=%v bytes=%d, want bytes moving and not finished",
			first[0].Done, first[0].BytesDone)
	}

	second, err := f.Transfers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !second[0].Done {
		t.Error("second observation: the transfer is still not finished")
	}
	if second[0].Path == "" {
		t.Error("a finished transfer has no path, so nothing can ingest it")
	}
}

// The content is DETERMINISTIC, so a demo can assert a digest.
//
// Random content cannot be asserted against, and #255 is what happens when a
// fixture's randomness is allowed to decide whether a test means anything.
func TestASimulatedFakeProducesTheSameBytesForTheSameSource(t *testing.T) {
	const src = "magnet:?xt=urn:btih:abc"
	var paths []string

	for range 2 {
		f := NewFake("sim", t.TempDir()).Simulate(2048)
		if _, err := f.Add(t.Context(), secret.Value(src)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Transfers(t.Context()); err != nil {
			t.Fatal(err)
		}
		done, err := f.Transfers(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, done[0].Path)
	}

	a, b := readAll(t, paths[0]), readAll(t, paths[1])
	if !equalBytes(a, b) {
		t.Error("two runs produced different bytes for the same source, so no demo " +
			"can assert a digest against it")
	}
	if len(a) != 2048 {
		t.Errorf("produced %d bytes, want the configured 2048", len(a))
	}
}

// And an UNSIMULATED fake still refuses an unseeded source, so Go tests keep
// their control.
func TestAnUnsimulatedFakeStillNeedsToBeTold(t *testing.T) {
	f := NewFake("manual", t.TempDir())
	_, err := f.Add(t.Context(), secret.Value("magnet:?xt=urn:btih:abc"))
	if err == nil {
		t.Fatal("an unsimulated fake invented content, so a test can no longer control it")
	}
	if errors.Is(err, ErrNotOurs) {
		t.Errorf("wrong refusal: %v", err)
	}
}

func readAll(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
