package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
)

type fakeStore struct {
	blob  Blob
	err   error
	calls []string
	trace *[]string
}

func (f *fakeStore) Link(_ context.Context, sourcePath string, mode Materialisation) (Blob, error) {
	f.calls = append(f.calls, sourcePath+"|"+string(mode))
	if f.trace != nil {
		*f.trace = append(*f.trace, "store.Link")
	}
	if f.err != nil {
		return Blob{}, f.err
	}
	b := f.blob
	b.Materialised = mode
	return b, nil
}

type fakeCatalog struct {
	root     Root
	rootErr  error
	peer     string
	peerErr  error
	result   Result
	recErr   error
	recorded []Recording
	trace    *[]string
}

func (f *fakeCatalog) SelfPeer(context.Context) (string, error) {
	if f.trace != nil {
		*f.trace = append(*f.trace, "catalog.SelfPeer")
	}
	if f.peerErr != nil {
		return "", f.peerErr
	}
	if f.peer == "" {
		return "peer-1", nil
	}
	return f.peer, nil
}

func (f *fakeCatalog) Root(_ context.Context, id string) (Root, error) {
	if f.trace != nil {
		*f.trace = append(*f.trace, "catalog.Root")
	}
	if f.rootErr != nil {
		return Root{}, f.rootErr
	}
	r := f.root
	r.ID = id
	return r, nil
}

func (f *fakeCatalog) Record(_ context.Context, rec Recording) (Result, error) {
	if f.trace != nil {
		*f.trace = append(*f.trace, "catalog.Record")
	}
	f.recorded = append(f.recorded, rec)
	if f.recErr != nil {
		return Result{}, f.recErr
	}
	return f.result, nil
}

type fakeIdentifier struct {
	candidate identification.Candidate
	seen      []string
}

func (f *fakeIdentifier) Identify(relPath, contentType string) identification.Candidate {
	f.seen = append(f.seen, relPath+"|"+contentType)
	return f.candidate
}

type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

func enabledRoot() Root {
	return Root{LibraryID: "lib-1", LibraryContentType: identification.Movie, Path: "/srv/movies", Mode: Reflink, Enabled: true}
}

func newPipeline(t *testing.T, store ByteStore, cat Catalog, ident Identifier) *Pipeline {
	t.Helper()
	p, err := New(Options{Store: store, Catalog: cat, Identifier: ident, Clock: fixedClock(time.Unix(1700000000, 0).UTC())})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// The ordering is the whole design. Bytes must be in the store before anything
// references them: a crash the other way round leaves a catalog row pointing at
// bytes that are not there, and only one of those two failures is recoverable
// without an operator.
func TestBytesAreMaterialisedBeforeAnythingIsRecorded(t *testing.T) {
	var trace []string
	store := &fakeStore{blob: Blob{Hash: "blake3:" + strings.Repeat("a", 64), Size: 42}, trace: &trace}
	cat := &fakeCatalog{root: enabledRoot(), trace: &trace}
	p := newPipeline(t, store, cat, &fakeIdentifier{})

	if _, err := p.Ingest(t.Context(), Request{RootID: "root-1", SourcePath: "/srv/movies/a.mkv", RelPath: "a.mkv"}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	link, record := indexOf(trace, "store.Link"), indexOf(trace, "catalog.Record")
	if link < 0 || record < 0 {
		t.Fatalf("expected both a Link and a Record, got %v", trace)
	}
	if link > record {
		t.Fatalf("bytes were recorded before they were stored: %v", trace)
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

// Identification failure must never be ingest failure (M1-11). An unparseable
// file lands under the synthetic Unidentified work and the ingest succeeds.
func TestAnUnidentifiableFileStillIngests(t *testing.T) {
	unidentified := identification.Unidentified(identification.ParsePath("weird.bin"))
	store := &fakeStore{blob: Blob{Hash: "blake3:" + strings.Repeat("b", 64), Size: 7}}
	cat := &fakeCatalog{root: enabledRoot(), result: Result{AssetID: "asset-1"}}
	p := newPipeline(t, store, cat, &fakeIdentifier{candidate: unidentified})

	if _, err := p.Ingest(t.Context(), Request{RootID: "root-1", SourcePath: "/srv/movies/weird.bin", RelPath: "weird.bin"}); err != nil {
		t.Fatalf("an unidentifiable file failed to ingest: %v", err)
	}
	if len(cat.recorded) != 1 {
		t.Fatalf("want one recording, got %d", len(cat.recorded))
	}
	if got := cat.recorded[0].Candidate.Source; got != identification.SourceUnidentified {
		t.Errorf("identification source = %q, want %q", got, identification.SourceUnidentified)
	}
}

func TestTheIdentifierSeesThePathRelativeToTheRootAndTheLibraryType(t *testing.T) {
	ident := &fakeIdentifier{}
	store := &fakeStore{blob: Blob{Hash: "blake3:" + strings.Repeat("c", 64)}}
	root := enabledRoot()
	root.LibraryContentType = identification.Series
	p := newPipeline(t, store, &fakeCatalog{root: root}, ident)

	if _, err := p.Ingest(t.Context(), Request{
		RootID: "root-1", SourcePath: "/srv/tv/Show/Season 01/S01E01.mkv", RelPath: "Show/Season 01/S01E01.mkv",
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	want := "Show/Season 01/S01E01.mkv|" + identification.Series
	if len(ident.seen) != 1 || ident.seen[0] != want {
		t.Fatalf("identifier saw %v, want [%q]", ident.seen, want)
	}
}

func TestTheRootsIngestModeReachesTheStore(t *testing.T) {
	for _, mode := range []Materialisation{Reflink, Hardlink, Copy} {
		t.Run(string(mode), func(t *testing.T) {
			store := &fakeStore{blob: Blob{Hash: "blake3:" + strings.Repeat("d", 64)}}
			root := enabledRoot()
			root.Mode = mode
			p := newPipeline(t, store, &fakeCatalog{root: root}, &fakeIdentifier{})
			if _, err := p.Ingest(t.Context(), Request{RootID: "r", SourcePath: "/x/a.mkv", RelPath: "a.mkv"}); err != nil {
				t.Fatal(err)
			}
			if len(store.calls) != 1 || !strings.HasSuffix(store.calls[0], "|"+string(mode)) {
				t.Fatalf("store saw %v, want mode %s", store.calls, mode)
			}
		})
	}
}

func TestRefusals(t *testing.T) {
	hash := "blake3:" + strings.Repeat("e", 64)
	tests := []struct {
		name string
		root Root
		req  Request
		want error
	}{
		{
			name: "a disabled root",
			root: Root{LibraryID: "lib", Mode: Reflink, Enabled: false},
			req:  Request{RootID: "r", SourcePath: "/x/a.mkv", RelPath: "a.mkv"},
			want: ErrRootDisabled,
		},
		{
			// ADR-0020: a linked asset has no blob at all. Milestone 1 only
			// ever writes managed assets, so this is refused rather than
			// half-implemented.
			name: "a linked root",
			root: Root{LibraryID: "lib", Mode: Link, Enabled: true},
			req:  Request{RootID: "r", SourcePath: "/x/a.mkv", RelPath: "a.mkv"},
			want: ErrLinkedRoot,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{blob: Blob{Hash: hash}}
			p := newPipeline(t, store, &fakeCatalog{root: tc.root}, &fakeIdentifier{})
			_, err := p.Ingest(t.Context(), tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if len(store.calls) != 0 {
				t.Errorf("bytes were moved for a refused ingest: %v", store.calls)
			}
		})
	}
}

func TestIncompleteRequestsAreRefusedBeforeAnythingMoves(t *testing.T) {
	tests := map[string]Request{
		"no root":     {SourcePath: "/x/a.mkv", RelPath: "a.mkv"},
		"no path":     {RootID: "r", RelPath: "a.mkv"},
		"no rel path": {RootID: "r", SourcePath: "/x/a.mkv"},
	}
	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			p := newPipeline(t, store, &fakeCatalog{root: enabledRoot()}, &fakeIdentifier{})
			if _, err := p.Ingest(t.Context(), req); err == nil {
				t.Fatal("an incomplete request was accepted")
			}
			if len(store.calls) != 0 {
				t.Errorf("bytes were moved for an invalid request: %v", store.calls)
			}
		})
	}
}

// A failed Record leaves the bytes in the store with nothing referencing them.
// That is the designed shape, not a leak: the GC reclaims an orphan after its
// grace window (ADR-0018), and the job is retried.
func TestARecordingFailureLeavesTheBytesStoredAndUnreferenced(t *testing.T) {
	boom := errors.New("boom")
	store := &fakeStore{blob: Blob{Hash: "blake3:" + strings.Repeat("f", 64), Size: 11}}
	cat := &fakeCatalog{root: enabledRoot(), recErr: boom}
	p := newPipeline(t, store, cat, &fakeIdentifier{})

	_, err := p.Ingest(t.Context(), Request{RootID: "r", SourcePath: "/x/a.mkv", RelPath: "a.mkv"})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
	if len(store.calls) != 1 {
		t.Fatalf("the bytes were not stored before the failure: %v", store.calls)
	}
}

func TestMIMEIsDerivedFromTheExtensionAndOverridable(t *testing.T) {
	store := &fakeStore{blob: Blob{Hash: "blake3:" + strings.Repeat("1", 64)}}
	cat := &fakeCatalog{root: enabledRoot()}
	p := newPipeline(t, store, cat, &fakeIdentifier{})

	if _, err := p.Ingest(t.Context(), Request{RootID: "r", SourcePath: "/x/a.mkv", RelPath: "Dir/a.mkv"}); err != nil {
		t.Fatal(err)
	}
	if got := cat.recorded[0].MIME; got != "video/x-matroska" {
		t.Errorf("derived MIME = %q, want video/x-matroska", got)
	}
	if got := cat.recorded[0].Filename; got != "a.mkv" {
		t.Errorf("filename = %q, want a.mkv", got)
	}

	if _, err := p.Ingest(t.Context(), Request{RootID: "r", SourcePath: "/x/b.mkv", RelPath: "b.mkv", MIME: "video/other"}); err != nil {
		t.Fatal(err)
	}
	if got := cat.recorded[1].MIME; got != "video/other" {
		t.Errorf("explicit MIME = %q, want video/other", got)
	}
}

// mime.TypeByExtension consults the host's mime database, so two peers would
// disagree about the same bytes depending on which packages are installed. The
// table is explicit for that reason (ADR-0017).
func TestMIMEForExtension(t *testing.T) {
	tests := map[string]string{
		".mkv":  "video/x-matroska",
		".MKV":  "video/x-matroska",
		".flac": "audio/flac",
		".epub": "application/epub+zip",
		".cbz":  "application/vnd.comicbook+zip",
		".srt":  "application/x-subrip",
		".zzz":  "",
		"":      "",
	}
	for ext, want := range tests {
		if got := MIMEForExtension(ext); got != want {
			t.Errorf("MIMEForExtension(%q) = %q, want %q", ext, got, want)
		}
	}
}

func TestBaseAndExt(t *testing.T) {
	tests := []struct{ rel, base, ext string }{
		{"a.mkv", "a.mkv", ".mkv"},
		{"Dir/Sub/a.b.mkv", "a.b.mkv", ".mkv"},
		{"Dir/noext", "noext", ""},
		{".hidden", ".hidden", ""},
		{"Dir/.hidden", ".hidden", ""},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := Base(tc.rel); got != tc.base {
			t.Errorf("Base(%q) = %q, want %q", tc.rel, got, tc.base)
		}
		if got := Ext(Base(tc.rel)); got != tc.ext {
			t.Errorf("Ext(Base(%q)) = %q, want %q", tc.rel, got, tc.ext)
		}
	}
}

func TestDedupeKeyIsStableAndPathScoped(t *testing.T) {
	if a, b := DedupeKey("r1", "a.mkv"), DedupeKey("r1", "a.mkv"); a != b {
		t.Fatalf("DedupeKey is not stable: %q vs %q", a, b)
	}
	if a, b := DedupeKey("r1", "a.mkv"), DedupeKey("r2", "a.mkv"); a == b {
		t.Fatal("two roots share a dedupe key — one root's ingest would suppress the other's")
	}
	if a, b := DedupeKey("r1", "a.mkv"), DedupeKey("r1", "b.mkv"); a == b {
		t.Fatal("two paths share a dedupe key")
	}
}

func TestNewRequiresItsPorts(t *testing.T) {
	store, cat, ident := &fakeStore{}, &fakeCatalog{}, &fakeIdentifier{}
	tests := map[string]Options{
		"no store":      {Catalog: cat, Identifier: ident},
		"no catalog":    {Store: store, Identifier: ident},
		"no identifier": {Store: store, Catalog: cat},
	}
	for name, opts := range tests {
		if _, err := New(opts); err == nil {
			t.Errorf("%s: New accepted an incomplete configuration", name)
		}
	}
}
