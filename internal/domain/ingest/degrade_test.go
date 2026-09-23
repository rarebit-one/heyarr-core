package ingest

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
)

// loggedPipeline is newPipeline with the log captured, because what this file
// asserts on IS the log: a degrade nobody is told about is the whole of #222.
func loggedPipeline(t *testing.T, store ByteStore, cat Catalog) (*Pipeline, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	p, err := New(Options{
		Store:      store,
		Catalog:    cat,
		Identifier: &fakeIdentifier{},
		Clock:      fixedClock(time.Unix(1700000000, 0).UTC()),
		Logger:     slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, &buf
}

func copyingStore() *fakeStore {
	return &fakeStore{
		blob:            Blob{Hash: "blake3:" + strings.Repeat("a", 64), Size: 42},
		materialisedAs:  Copy,
		degradedBecause: "reflink: operation not supported; hardlink: invalid cross-device link",
	}
}

// The ladder falling all the way to a byte copy is reported at WARNING, not
// buried in an INFO line shaped exactly like a healthy one.
//
// #222 was 63 consecutive `materialised=copy` lines at INFO that nobody read
// until 22 GB had gone, so "it was in the log" is a standard this already met
// while being invisible.
func TestADegradeToACopyIsWarnedAboutAndNamesTheReason(t *testing.T) {
	tests := []struct {
		name      string
		requested Materialisation
		wantWarn  bool
	}{
		// Asked for the cheap rung, got a copy: the operator's configuration
		// is not being honoured and the cost is a second copy of everything.
		{"reflink degraded to copy", Reflink, true},
		{"hardlink degraded to copy", Hardlink, true},
		// Asked for a copy, got a copy. Nothing degraded, so warning would be
		// scolding somebody for their own deliberate choice.
		{"copy was what was asked for", Copy, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := enabledRoot()
			root.Mode = tt.requested
			p, buf := loggedPipeline(t, copyingStore(), &fakeCatalog{root: root})

			if _, err := p.Ingest(t.Context(), Request{
				RootID: "root-1", SourcePath: "/srv/movies/a.mkv", RelPath: "a.mkv",
			}); err != nil {
				t.Fatalf("Ingest: %v", err)
			}

			warned := strings.Contains(buf.String(), `level=WARN`) &&
				strings.Contains(buf.String(), "fell back to COPYING")
			if warned != tt.wantWarn {
				t.Fatalf("warned = %v, want %v; log:\n%s", warned, tt.wantWarn, buf.String())
			}
			if !tt.wantWarn {
				return
			}
			// The reason, not just the fact. A copy with no reason is
			// indistinguishable from a copy for a completely different
			// reason, which is precisely how this hid.
			if !strings.Contains(buf.String(), "invalid cross-device link") {
				t.Errorf("the warning does not carry the errno the ladder degraded on:\n%s", buf.String())
			}
			if !strings.Contains(buf.String(), "requested="+string(tt.requested)) {
				t.Errorf("the warning does not say what was asked for:\n%s", buf.String())
			}
		})
	}
}

// Once per process. The condition is a property of the deployment, so the
// second thousand warnings say nothing the first did not — and a log nobody
// can read is the failure mode this is fixing, not the fix.
func TestTheDegradeWarningIsRaisedOnceAndEveryIngestLineStillCarriesTheReason(t *testing.T) {
	p, buf := loggedPipeline(t, copyingStore(), &fakeCatalog{root: enabledRoot()})

	for _, name := range []string{"a.mkv", "b.mkv", "c.mkv"} {
		if _, err := p.Ingest(t.Context(), Request{
			RootID: "root-1", SourcePath: "/srv/movies/" + name, RelPath: name,
		}); err != nil {
			t.Fatalf("Ingest %s: %v", name, err)
		}
	}

	if n := strings.Count(buf.String(), "fell back to COPYING"); n != 1 {
		t.Errorf("the degrade was warned about %d times over three ingests, want 1:\n%s", n, buf.String())
	}
	// The per-file record is what makes counting possible after the fact, so
	// it must survive the warning being deduplicated.
	if n := strings.Count(buf.String(), "degraded_because="); n != 4 {
		t.Errorf("degraded_because appears %d times, want 4 (one warning + one per ingest):\n%s",
			n, buf.String())
	}
}

// The other half of the same honesty: a healthy ingest must NOT carry an empty
// degraded_because. The field is what an operator greps for, and a field
// present on every line is not a signal — this is the log-shape equivalent of
// a check that has never rejected anything.
func TestAHealthyIngestLineCarriesNoDegradedBecause(t *testing.T) {
	store := &fakeStore{blob: Blob{Hash: "blake3:" + strings.Repeat("a", 64), Size: 42}}
	root := enabledRoot()
	root.Mode = Hardlink
	root.LibraryContentType = identification.Movie
	// The rung on the ingest LINE comes back from the catalog's Result, which
	// is why the fake has to report it here.
	cat := &fakeCatalog{root: root, result: Result{Materialised: Hardlink}}
	p, buf := loggedPipeline(t, store, cat)

	if _, err := p.Ingest(t.Context(), Request{
		RootID: "root-1", SourcePath: "/srv/movies/a.mkv", RelPath: "a.mkv",
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if strings.Contains(buf.String(), "degraded_because") {
		t.Errorf("a healthy hardlink ingest logged degraded_because:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "materialised=hardlink") {
		t.Errorf("the ingest line does not name the rung that was reached:\n%s", buf.String())
	}
}
