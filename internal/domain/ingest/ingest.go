// Package ingest orchestrates the ingest pipeline — the single path by which
// bytes come under Heyarr management (spec §65, §66).
//
// Everything that produces bytes ends here: a completed acquisition, a
// scanner, an upload, a rip, another peer. That is why the pipeline lives in
// the domain and owns the ordering, while the byte store, the catalog and the
// identifier are ports it depends on and never names concretely (§18,
// ADR-0006, ADR-0007).
//
// The ordering is load-bearing. Bytes are materialised into the store BEFORE
// anything is recorded, so a crash between the two leaves an orphaned file the
// garbage collector reclaims (ADR-0018) rather than a catalog row pointing at
// bytes that are not there. The reverse order trades a reclaimable orphan for a
// dangling reference, and only one of those is recoverable without an operator.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/publication"
)

// JobType is the queue's name for this work. The scanner, the acquisition
// pipeline and the CLI all enqueue it; none of them may spell it themselves.
const JobType = "ingest_artifact"

// Payload is the ingest_artifact job's payload — the wire contract between
// whatever found the bytes and the handler that brings them under management.
// The scanner (M1-12), the acquisition pipeline (M3) and the CLI all produce
// one; none of them may invent its own shape.
type Payload struct {
	// RootID names the library root the file was found under.
	RootID string `json:"root_id"`
	// Path is the absolute path on the ingesting host.
	Path string `json:"path"`
	// RelPath is Path relative to the root, slash-separated, and is what
	// identification parses. It is carried rather than recomputed so that the
	// scanner's notion of "relative to the root" is the one that is used.
	RelPath string `json:"rel_path"`
	// MIME, when set, overrides extension-based derivation.
	MIME string `json:"mime,omitempty"`
}

// DedupeKey is the job queue's idempotency key for one path under one root.
// Enqueueing the same file twice while the first ingest is still live yields
// one job, not two (ADR-0008).
func DedupeKey(rootID, relPath string) string { return "ingest:" + rootID + ":" + relPath }

// Materialisation is how a source file becomes a blob. It mirrors the storage
// fabric's ladder without importing it: the domain states the intent, the
// fabric decides what the filesystem will actually oblige (ADR-0014).
type Materialisation string

// The ladder, cheapest first.
const (
	// Reflink tries copy-on-write cloning, then hardlink, then a byte copy.
	Reflink Materialisation = "reflink"
	// Hardlink shares the inode with the source.
	Hardlink Materialisation = "hardlink"
	// Copy always duplicates the bytes.
	Copy Materialisation = "copy"
	// Link catalogues files where they are and never copies them, producing a
	// linked asset with no blob at all (ADR-0020). Milestone 1 never writes
	// one; see ErrLinkedRoot.
	Link Materialisation = "link"
	// None is an OUTCOME only, never an intent: the store already held these
	// bytes, so nothing was materialised and no rung was reached.
	//
	// It exists because reporting the requested rung for a deduplicating
	// ingest made every such ingest on a filesystem without block cloning
	// claim `reflink` — the one value that filesystem can never produce
	// (#223). Pair it with Result.Deduplicated, which says why.
	None Materialisation = "none"
)

// Blob is what the byte store reports after materialising bytes. Deliberately
// not the store's own descriptor type: the domain does not import the store.
type Blob struct {
	// Hash is the canonical byte identity, "blake3:<64 lowercase hex>"
	// (ADR-0005). Nothing else in this package is identity.
	Hash string
	Size int64
	// Materialised is how the bytes actually arrived, after the ladder
	// degraded to whatever the filesystem supported.
	Materialised Materialisation
	// Deduplicated reports that the store already held these bytes, so nothing
	// was written.
	Deduplicated bool
	// DegradedBecause says why Materialised is not a higher rung, and is empty
	// when the best available one was reached.
	//
	// Carried through to the ingest log because that is where somebody looks.
	// #222 was 63 files copied where hardlink should have worked, with the
	// errno discarded 63 times — the ladder degrading is ordinary, degrading
	// SILENTLY is what made a 22 GB surprise take an experiment to explain.
	DegradedBecause string
}

// Root is a library root's ingest configuration, as the pipeline needs it.
type Root struct {
	ID                 string
	LibraryID          string
	LibraryContentType string
	Path               string
	Mode               Materialisation
	Enabled            bool
}

// Recording is everything the catalog must persist for one ingest, in one
// transaction. The pipeline assembles it; the catalog decides how it is stored.
type Recording struct {
	Root       Root
	Candidate  identification.Candidate
	Blob       Blob
	SourcePath string
	RelPath    string
	Filename   string
	MIME       string
	PeerID     string
	Now        time.Time
	// Publication is what the container declared about itself, when it is one
	// of §69's four and its index was readable. Nil otherwise, which covers
	// both "not a publication" and "a publication we do not index".
	Publication *publication.Info
}

// Result reports what one ingest actually changed. The Created flags are what
// make idempotency observable: a re-run reports the same identifiers with every
// flag false.
type Result struct {
	BlobHash     string
	BlobSize     int64
	BlobCreated  bool
	Materialised Materialisation
	// DegradedBecause says why Materialised is not a higher rung (#222).
	DegradedBecause string
	// Deduplicated reports that the bytes were already under management. It is
	// exactly !BlobCreated, and it is the flag carried on ingest.completed.
	Deduplicated   bool
	WorkID         string
	WorkCreated    bool
	EditionID      string
	EditionCreated bool
	AssetID        string
	AssetCreated   bool
	ReplicaCreated bool
}

// ByteStore materialises a source file into content-addressed storage. The
// domain never learns where the bytes went or how they are laid out (§18).
type ByteStore interface {
	Link(ctx context.Context, sourcePath string, mode Materialisation) (Blob, error)

	// OpenBlob returns random access to stored bytes, for reading a
	// container's own index (§69). The caller closes it.
	//
	// It is on this port rather than a second one because the domain's
	// question is the same either way — "give me these bytes" — and a
	// publication examiner that took its own storage interface would be a
	// second place to answer where bytes live.
	OpenBlob(ctx context.Context, hash string) (ReaderAtCloser, int64, error)
}

// ReaderAtCloser is random access to a blob. It is io.ReaderAt rather than
// io.Reader because a ZIP's central directory is at the END of the file, so a
// sequential reader would mean materialising the whole archive to count its
// entries.
type ReaderAtCloser interface {
	io.ReaderAt
	io.Closer
}

// Identifier turns a path into a content candidate. It never fails: an
// unparseable path yields the synthetic Unidentified candidate, because
// identification failure must never be ingest failure (M1-11).
type Identifier interface {
	Identify(relPath, libraryContentType string) identification.Candidate
}

// Catalog is the control plane's record of what exists.
//
// Record is deliberately one call rather than a transaction handle the pipeline
// drives: the atomicity requirement belongs to the implementation, and a domain
// that could begin and commit transactions would be a domain that knows how it
// is persisted.
type Catalog interface {
	// SelfPeer returns this node's peer identity. There is exactly one peer in
	// Milestone 1 and the row exists anyway (ADR-0010).
	SelfPeer(ctx context.Context) (string, error)
	// Root returns a library root's ingest configuration.
	Root(ctx context.Context, rootID string) (Root, error)
	// Record commits the blob, work, edition, asset, replica and the resulting
	// events atomically, and is safely re-runnable.
	Record(ctx context.Context, rec Recording) (Result, error)
}

// Clock is injected so ingest is testable without wall time (ADR-0017).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Errors the pipeline returns.
var (
	// ErrRootDisabled means the root exists but ingest is switched off for it.
	ErrRootDisabled = errors.New("ingest: library root is disabled")
	// ErrLinkedRoot means the root catalogues files in place. Linked assets
	// have no blob at all (ADR-0020) and nothing has ever written one, so this
	// is refused rather than half-implemented.
	//
	// The message used to say "before Milestone 5". Milestone 5 has been and
	// gone without answering ADR-0020's question — it made replication cheaper,
	// which is a different question — so the milestone is dropped rather than
	// advanced to the next one. A date in an error message that keeps moving is
	// a promise nobody made.
	ErrLinkedRoot = errors.New("ingest: linked roots are not supported (ADR-0020)")
	// ErrRootNotFound means no such library root.
	ErrRootNotFound = errors.New("ingest: library root not found")
)

// Request is one artifact to bring under management.
type Request struct {
	// RootID names the library root the file was found under.
	RootID string
	// SourcePath is the absolute path on the ingesting host. It is provenance,
	// never identity (§61).
	SourcePath string
	// RelPath is SourcePath relative to the root, slash-separated. It is what
	// identification parses.
	RelPath string
	// MIME, when empty, is derived from the extension.
	MIME string
	// Work, when set, is the Work this file belongs to — and it OVERRIDES what
	// the path heuristic would have concluded.
	//
	// # Why an override exists at all
	//
	// Identification parses RelPath because a library scan has nothing else:
	// nobody asked for those files, so a guess from their shape is the honest
	// best answer. An acquisition is the opposite situation. Something asked
	// for this, by Work id, and re-deriving identity from the downloaded
	// filename throws away the one fact that was certain.
	//
	// The two disagree ROUTINELY — release titles carry extensions, scene tags
	// and the indexer's own normalisation — so the case where the guess happens
	// to agree is the lucky one, not the normal one. When they disagree the
	// asset lands on a second, path-derived Work and the want that asked for it
	// reports `assets: []` forever, in a state indistinguishable from patience
	// (#224).
	//
	// It overrides the WORK identity only. Everything the path legitimately
	// knows better — the edition, the quality label, the asset's role, the
	// language — is per-FILE and still comes from the heuristic, because the
	// want names what was wanted and not which of its editions arrived.
	Work *WorkOverride
}

// WorkOverride names the Work an asset must attach to, from a caller that
// knows rather than guesses.
type WorkOverride struct {
	ContentType string
	WorkKey     string
	Title       string
	SortTitle   string
	Year        int
}

// Options configure a Pipeline.
type Options struct {
	Store      ByteStore
	Catalog    Catalog
	Identifier Identifier
	Clock      Clock
	Logger     *slog.Logger
}

// Pipeline runs the ingest sequence.
type Pipeline struct {
	store  ByteStore
	cat    Catalog
	ident  Identifier
	clock  Clock
	logger *slog.Logger
}

// New constructs a Pipeline.
func New(opts Options) (*Pipeline, error) {
	if opts.Store == nil {
		return nil, errors.New("ingest: a byte store is required")
	}
	if opts.Catalog == nil {
		return nil, errors.New("ingest: a catalog is required")
	}
	if opts.Identifier == nil {
		return nil, errors.New("ingest: an identifier is required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Pipeline{store: opts.Store, cat: opts.Catalog, ident: opts.Identifier, clock: clock, logger: logger}, nil
}

// Ingest brings one artifact under management.
//
// The sequence is: resolve the root, identify, materialise the bytes, then
// record. Materialising before recording is what makes a mid-flight crash
// recoverable — see the package comment.
func (p *Pipeline) Ingest(ctx context.Context, req Request) (Result, error) {
	if req.RootID == "" {
		return Result{}, errors.New("ingest: root_id must be set")
	}
	if req.SourcePath == "" {
		return Result{}, errors.New("ingest: source path must be set")
	}
	if req.RelPath == "" {
		return Result{}, errors.New("ingest: relative path must be set")
	}

	root, err := p.cat.Root(ctx, req.RootID)
	if err != nil {
		return Result{}, err
	}
	if !root.Enabled {
		return Result{}, fmt.Errorf("%w: %s", ErrRootDisabled, root.ID)
	}
	if root.Mode == Link {
		return Result{}, fmt.Errorf("%w: %s", ErrLinkedRoot, root.ID)
	}
	mode := root.Mode
	if mode == "" {
		mode = Reflink
	}

	// Identification comes before the bytes move, so that a file nothing can
	// parse still ingests — under the synthetic Unidentified work — rather than
	// failing the job and being retried five times to no purpose (M1-11).
	candidate := p.ident.Identify(req.RelPath, root.LibraryContentType)
	// A caller that KNOWS which Work this is overrides the guess. See
	// Request.Work — this is the whole of #224.
	if req.Work != nil {
		candidate = applyWorkOverride(candidate, *req.Work)
	}

	peerID, err := p.cat.SelfPeer(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("ingest: resolving this peer: %w", err)
	}

	blob, err := p.store.Link(ctx, req.SourcePath, mode)
	if err != nil {
		return Result{}, fmt.Errorf("ingest: materialising %s: %w", req.SourcePath, err)
	}

	filename := Base(req.RelPath)
	mimeType := req.MIME
	if mimeType == "" {
		mimeType = MIMEForExtension(Ext(filename))
	}

	// §66 puts examination inside the pipeline, between materialisation and
	// recording. It happens AFTER the bytes are in the store rather than
	// against the source file, so that what is examined is what is managed —
	// on a hardlink-ingested root the two are the same file, and on a copied one they
	// need not be.
	//
	// A container that cannot be read is not an ingest failure. Heyarr stores
	// bytes it cannot interpret; that is the premise, and a comic with a
	// corrupt index is still a comic worth having.
	pubInfo := p.examinePublication(ctx, blob.Hash, filename)

	res, err := p.cat.Record(ctx, Recording{
		Root:        root,
		Candidate:   candidate,
		Blob:        blob,
		SourcePath:  req.SourcePath,
		RelPath:     req.RelPath,
		Filename:    filename,
		MIME:        mimeType,
		PeerID:      peerID,
		Now:         p.clock.Now(),
		Publication: pubInfo,
	})
	if err != nil {
		// The bytes are in the store and nothing references them. That is the
		// designed failure shape: the garbage collector reclaims an orphan
		// after its grace window (ADR-0018), and the job will be retried.
		return Result{}, fmt.Errorf("ingest: recording %s: %w", req.SourcePath, err)
	}
	// Set here rather than passed through Recording: it is a fact about HOW the
	// bytes arrived, not about the catalogue, and the catalogue has no use for
	// it. Threading it through the persistence layer would make every store
	// implementation carry a field none of them reads.
	res.DegradedBecause = blob.DegradedBecause

	p.logger.Info("ingested",
		"source_path", req.SourcePath,
		"blob", res.BlobHash,
		"size", res.BlobSize,
		"materialised", string(res.Materialised),
		// Only when there is something to say. A field that is empty on every
		// healthy ingest would be noise on every line, and this one is meant
		// to be noticed.
		slog.String("degraded_because", res.DegradedBecause),
		"deduplicated", res.Deduplicated,
		"asset", res.AssetID,
		"asset_created", res.AssetCreated,
		"work", res.WorkID,
		"identification", candidate.Source,
		"rule", candidate.Rule)
	return res, nil
}

// examinePublication reads a publication container's own index, or returns nil.
//
// Every failure path here is a nil and a log line, never an error. The only
// thing that could go wrong is that Heyarr knows slightly less about a file it
// has nonetheless stored, hashed, replicated and can serve — and failing the
// ingest over that would be trading a whole asset for a page count.
func (p *Pipeline) examinePublication(ctx context.Context, hash, filename string) *publication.Info {
	format := publication.FormatForExtension(Ext(filename))
	if format == "" {
		return nil
	}
	if !format.Indexed() {
		// Recognised, deliberately not read. It is still recorded as a
		// publication of that format — "we know what this is and did not count
		// it" is a different and more useful answer than silence.
		return &publication.Info{Format: format}
	}

	r, size, err := p.store.OpenBlob(ctx, hash)
	if err != nil {
		p.logger.Warn("a publication could not be opened for examination",
			"blob", hash, "format", string(format), "error", err)
		return &publication.Info{Format: format}
	}
	defer func() { _ = r.Close() }()

	info, err := publication.Examine(r, size, format)
	if err != nil {
		p.logger.Warn("a publication container could not be read; it is stored anyway",
			"blob", hash, "format", string(format), "error", err)
		return &publication.Info{Format: format}
	}
	return &info
}

// Base is the final element of a slash-separated relative path. The domain may
// not import path/filepath (ADR-0006) and would not want to anyway: these are
// always forward-slashed, whatever the host separator is.
func Base(relPath string) string {
	if i := strings.LastIndexByte(relPath, '/'); i >= 0 {
		return relPath[i+1:]
	}
	return relPath
}

// Ext is the lowercased extension of a filename, including the dot.
func Ext(filename string) string {
	i := strings.LastIndexByte(filename, '.')
	if i <= 0 {
		return ""
	}
	return strings.ToLower(filename[i:])
}

// mimeByExtension is deliberately an explicit table rather than
// mime.TypeByExtension, which consults the host's mime database: two peers
// would then disagree about the same bytes depending on which packages happen
// to be installed. ADR-0017 wants determinism, and a catalog is a thing two
// peers must agree about.
var mimeByExtension = map[string]string{
	".mkv": "video/x-matroska", ".mp4": "video/mp4", ".m4v": "video/x-m4v",
	".avi": "video/x-msvideo", ".mov": "video/quicktime", ".webm": "video/webm",
	".ts": "video/mp2t", ".m2ts": "video/mp2t", ".wmv": "video/x-ms-wmv",
	".mpg": "video/mpeg", ".mpeg": "video/mpeg",

	".mp3": "audio/mpeg", ".flac": "audio/flac", ".m4a": "audio/mp4",
	".ogg": "audio/ogg", ".opus": "audio/opus", ".wav": "audio/wav",
	".aac": "audio/aac", ".wma": "audio/x-ms-wma", ".alac": "audio/mp4",

	".epub": "application/epub+zip", ".pdf": "application/pdf",
	".mobi": "application/x-mobipocket-ebook", ".azw3": "application/vnd.amazon.ebook",
	".cbz": "application/vnd.comicbook+zip", ".cbr": "application/vnd.comicbook-rar",
	".txt": "text/plain",

	".srt": "application/x-subrip", ".ass": "text/x-ssa", ".ssa": "text/x-ssa",
	".vtt": "text/vtt", ".sub": "text/plain", ".idx": "text/plain",

	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".webp": "image/webp", ".gif": "image/gif",

	".nfo": "text/plain", ".xml": "application/xml", ".json": "application/json",
}

// MIMEForExtension maps a lowercased extension to a media type, or "" when the
// extension is not one Heyarr recognises. An unknown type is recorded as
// unknown rather than guessed: probing in Milestone 2 can correct it, and a
// wrong media type is harder to notice than a missing one.
func MIMEForExtension(ext string) string { return mimeByExtension[strings.ToLower(ext)] }

// applyWorkOverride replaces a candidate's WORK identity with a known one.
//
// # What it deliberately does not touch
//
// EditionKey, EditionLabel, EditionType, EditionAttributes, Language and
// AssetRole all survive. Those are facts about the FILE — which cut, which
// resolution, whether this is a subtitle — and the path is the better source
// for every one of them. The want names what was wanted; it does not name which
// of its editions turned up.
//
// Identified becomes true and Source records that a person's declared want, not
// a path shape, is why this asset is where it is. That distinction is the same
// one identification_source exists to carry, and without it an operator reading
// the asset would be told a heuristic matched when none did.
//
// Rule is CLEARED when the path heuristic did not itself identify the file:
// keeping a rule name beside a source of "desired-item" would name a heuristic
// that had no part in the answer. Where the heuristic DID match, the rule is
// kept — it is true, and it is the trace of what the path would have said.
func applyWorkOverride(c identification.Candidate, w WorkOverride) identification.Candidate {
	if !c.Identified {
		c.Rule = ""
	}
	c.ContentType = w.ContentType
	c.WorkKey = w.WorkKey
	c.Title = w.Title
	c.SortTitle = w.SortTitle
	c.Year = w.Year
	c.Source = identification.SourceDesiredItem
	c.Identified = true
	return c
}
