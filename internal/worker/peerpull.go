package worker

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// Peer-pull of non-swarm assets (ADR-0119 amendment, #833).
//
// One pass does three things, in order, for the libraries this node opted in:
//
//   - REPORT. Each sibling's acquire this node recorded `no_site_source` for
//     (a dead swarm, ADR-0119 #800 amendment) gets an `unsourced` op, once,
//     so its origin holds the asset for this node.
//   - PUBLISH. Each asset no swarm covers (a scan, an HTTP, usenet or direct
//     fetch) gets a `hold` op naming its blob, once per (work, blob). An
//     acquire this site signed stops covering its asset once a sibling has
//     reported it unsourced.
//   - APPLY. Each sibling's hold is landed in an opted-in library here: the
//     blob is recorded as op-sourced (the signer's claim), pulled from the
//     signer by the ordinary replicate_blob, and once it is held, recorded as
//     an asset under the origin's work and edition keys.
//
// Every outcome is recorded per op hash with an event; `own` and `held` are
// final, the rest are re-evaluated on the next pass. The pass is idempotent:
// it re-reads everything it acts on, and a re-run publishes, queues and
// records nothing new (invariant 9).

const (
	// peerPullRetryAfter spaces a pull that failed: the next one is not queued
	// until this long after the failure was recorded. A pull's own retries are
	// the queue's; this bounds how often the link carries a blob that keeps
	// failing.
	peerPullRetryAfter = time.Hour
	// maxPeerPullsPerPass bounds how many pulls one pass queues. The transfer
	// lane runs two at a time anyway; the bound keeps a large first pass from
	// filling the queue with thousands of jobs at once.
	maxPeerPullsPerPass = 64
	// maxHoldBatch is how many hold ops are recorded per transaction.
	maxHoldBatch = 200
	// maxOpLogBytes bounds the catalog op-log the publisher will grow. The
	// whole log rides one catalog-sync body, capped at 8 MiB at both ends; a
	// log past it would stop the pair converging, tombstones included. Holds
	// are not published beyond this, with headroom for the other kinds.
	maxOpLogBytes = 6 << 20
)

// PeerPullOps is the op-log the pass reads and appends to; catalogtomb.Store
// satisfies it.
type PeerPullOps interface {
	Ops(ctx context.Context) ([]string, error)
	RecordOps(ctx context.Context, ops []string) error
}

// PeerPullCatalog is the part of the catalog the pass uses.
type PeerPullCatalog interface {
	HoldCandidates(ctx context.Context, libraries []string, maxBytes int64) ([]catalog.HoldCandidate, error)
	PeerPullLibraries(ctx context.Context, names []string) ([]catalog.PeerPullLibrary, error)
	HoldApplications(ctx context.Context) (map[string]catalog.HoldApplication, error)
	RecordHoldApplication(ctx context.Context, a catalog.HoldApplication) error
	LibraryHoldsBlob(ctx context.Context, libraryID, blobHash string) (bool, error)
	RecordOpSourcedBlob(ctx context.Context, b catalog.OpSourcedBlob) (string, error)
	LastFailedPull(ctx context.Context, blobHash string) (time.Time, error)
	SelfPeer(ctx context.Context) (string, error)
	FanOutApplications(ctx context.Context) (map[string]catalog.FanOutApplication, error)
	IsSiblingKey(ctx context.Context, key string) (bool, error)
}

// BlobHolder reports whether this node's content store holds a blob.
type BlobHolder interface {
	Has(ctx context.Context, hash hashing.Hash) (bool, error)
}

// Adopter records held bytes as an asset; *ingest.Pipeline satisfies it.
type Adopter interface {
	Adopt(ctx context.Context, req ingest.AdoptRequest) (ingest.Result, error)
}

// PeerPull is what the pass needs.
type PeerPull struct {
	Ops     PeerPullOps
	Catalog PeerPullCatalog
	Store   BlobHolder
	Adopter Adopter
	Queue   ProbeEnqueuer
	// Signer is this peer's identity key (ADR-0012): holds are signed with it,
	// and its public half recognises this node's own ops.
	Signer ed25519.PrivateKey
	// Site is this node's site name (peer.site), named as a hold's origin.
	Site string
	// Libraries are the opted-in library names (acquisition.peer_pull).
	Libraries []string
	// MaxBlobBytes is the per-blob cap.
	MaxBlobBytes int64
	Now          func() time.Time
	Log          *slog.Logger
}

// PeerPullHandler runs one pass. A pass with no library opted in does nothing
// at all: it is never registered then, and this is the backstop.
func PeerPullHandler(p PeerPull) HandlerFunc {
	return func(ctx context.Context, _ jobs.Job) error {
		if len(p.Libraries) == 0 || len(p.Signer) != ed25519.PrivateKeySize {
			return nil
		}
		log := p.Log
		if log == nil {
			log = slog.New(slog.DiscardHandler)
		}
		now := time.Now
		if p.Now != nil {
			now = p.Now
		}
		selfKey := identity.FormatPublicKey(p.Signer.Public().(ed25519.PublicKey))

		tokens, err := p.Ops.Ops(ctx)
		if err != nil {
			return err
		}
		reported, err := p.report(ctx, tokens, selfKey, now().UTC(), log)
		if err != nil {
			return err
		}
		if reported > 0 {
			if tokens, err = p.Ops.Ops(ctx); err != nil {
				return err
			}
		}
		published, err := p.publish(ctx, tokens, selfKey, now().UTC(), log)
		if err != nil {
			return err
		}
		if published > 0 {
			if tokens, err = p.Ops.Ops(ctx); err != nil {
				return err
			}
		}
		return p.apply(ctx, catalogop.Evaluate(tokens), selfKey, now().UTC(), log)
	}
}

// report signs an unsourced op for each effective acquire a sibling signed
// that this node recorded `no_site_source` for, in a content type an opted-in
// library here takes, whose release fits the cap when the op gives its size,
// and that this node has not reported already (ADR-0119 #800 amendment). It
// returns how many it wrote.
func (p PeerPull) report(ctx context.Context, tokens []string, selfKey string, now time.Time, log *slog.Logger) (int, error) {
	view := catalogop.Evaluate(tokens)
	if len(view.Acquisitions) == 0 {
		return 0, nil
	}
	apps, err := p.Catalog.FanOutApplications(ctx)
	if err != nil {
		return 0, err
	}
	libs, err := p.Catalog.PeerPullLibraries(ctx, p.Libraries)
	if err != nil {
		return 0, err
	}
	types := map[string]bool{}
	for _, l := range libs {
		types[l.ContentType] = true
	}
	done := map[string]bool{}
	for _, op := range view.Accepted {
		if op.Kind == catalogop.OpUnsourced && op.By == selfKey {
			done[op.Unsourced.Acquire] = true
		}
	}
	var due []catalogop.Op
	for _, op := range view.Acquisitions {
		app, ok := apps[op.Hash]
		switch {
		case op.By == selfKey || done[op.Hash] || !types[op.Target.ContentType]:
		case !ok || app.Outcome != catalog.FanOutNoSiteSource:
		case op.Acquire.Size > p.MaxBlobBytes:
			// The origin would not publish it, and the link stays unused.
		default:
			due = append(due, op)
		}
	}
	if len(due) == 0 {
		return 0, nil
	}
	sort.Slice(due, func(i, j int) bool { return due[i].Hash < due[j].Hash })

	logBytes := opLogBytes(tokens)
	site := p.siteName()
	// Each report cites the one before it, as holds do, leaving room for the
	// acquire it names.
	heads := view.Heads
	if len(heads) > catalogop.MaxPrev-1 {
		heads = heads[len(heads)-(catalogop.MaxPrev-1):]
	}
	var batch []string
	for _, op := range due {
		tok, err := catalogop.SignUnsourced(p.Signer, op.Target.ContentType, op.Target.WorkKey,
			catalogop.Unsourced{Acquire: op.Hash, Site: site}, heads, now)
		if err != nil {
			return 0, fmt.Errorf("worker: signing the report for acquire %s: %w", op.Hash, err)
		}
		if logBytes+len(tok) > maxOpLogBytes {
			log.Warn("not reporting further unsourced releases: the catalog op-log is near the catalog-sync limit",
				"log_bytes", logBytes, "limit", maxOpLogBytes)
			break
		}
		logBytes += len(tok)
		heads = []string{catalogop.Hash(tok)}
		batch = append(batch, tok)
	}
	if len(batch) == 0 {
		return 0, nil
	}
	if err := p.Ops.RecordOps(ctx, batch); err != nil {
		return 0, fmt.Errorf("worker: recording unsourced ops: %w", err)
	}
	log.Info("reported siblings' releases this site cannot source, for their origins to hold", "reported", len(batch))
	return len(batch), nil
}

// publish signs a hold for every candidate asset no swarm covers and this node
// has not already published, and records them. It returns how many it wrote.
func (p PeerPull) publish(ctx context.Context, tokens []string, selfKey string, now time.Time, log *slog.Logger) (int, error) {
	cands, err := p.Catalog.HoldCandidates(ctx, p.Libraries, p.MaxBlobBytes)
	if err != nil || len(cands) == 0 {
		return 0, err
	}
	view := catalogop.Evaluate(tokens)
	unsourced, err := p.reportedBySibling(ctx, view, selfKey)
	if err != nil {
		return 0, err
	}
	published := map[string]bool{}
	covered := map[string]bool{}
	for _, op := range view.Accepted {
		switch {
		case op.Kind == catalogop.OpHold && op.By == selfKey:
			published[catalogop.HoldKey(op.Target, op.Hold.Blob)] = true
		case op.Kind == catalogop.OpAcquire && op.By == selfKey &&
			unsourced[catalogop.AcquireKey(op.Target, op.Acquire.Scope, op.Acquire.Aspect)]:
			// This site's selection, whose effective acquire a sibling
			// reported it cannot source (a dead swarm, #800): neither it nor an
			// earlier one for the same scope covers the asset any longer, which
			// is held for the sibling instead.
		case op.Kind == catalogop.OpAcquire:
			// A swarm covers it: an acquire, this site's or a sibling's, names
			// the work, the edition or the item. Its bytes reach the sibling
			// through the sibling's own client, and the link stays unused.
			covered[op.Target.Key()+"\x1f"+op.Acquire.Scope] = true
		}
	}
	logBytes := opLogBytes(tokens)
	site := p.siteName()

	// Each hold cites the one before it, so the log keeps a single head: a
	// batch citing the same heads would leave every hold a head, and the next
	// op signed here would cite more than catalogop.MaxPrev of them.
	heads := view.Heads
	if len(heads) > catalogop.MaxPrev {
		heads = heads[len(heads)-catalogop.MaxPrev:]
	}
	var batch []string
	written, skipped := 0, 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := p.Ops.RecordOps(ctx, batch); err != nil {
			return fmt.Errorf("worker: recording hold ops: %w", err)
		}
		written += len(batch)
		batch = batch[:0]
		return nil
	}
	for _, c := range cands {
		if strings.HasPrefix(c.WorkKey, "music-recording:") {
			continue // old peers cannot enforce recording evidence
		}
		target := catalogop.Target{ContentType: c.ContentType, WorkKey: c.WorkKey}
		if view.IsTombstoned(target) || published[catalogop.HoldKey(target, c.BlobHash)] {
			continue
		}
		if covered[target.Key()+"\x1f"+catalogop.ScopeWork] ||
			covered[target.Key()+"\x1f"+catalogop.ScopeEditionPrefix+c.EditionKey] ||
			(c.ItemKey != "" && covered[target.Key()+"\x1f"+catalogop.ScopeItemPrefix+c.ItemKey]) {
			continue
		}
		if c.EditionKey == "" {
			skipped++
			continue
		}
		heldAt := c.HeldAt
		if heldAt.IsZero() {
			heldAt = now
		}
		tok, err := catalogop.SignHold(p.Signer, c.ContentType, c.WorkKey, catalogop.Hold{
			Scope:     catalogop.ScopeEditionPrefix + c.EditionKey,
			Blob:      c.BlobHash,
			Size:      c.Size,
			Path:      c.RelPath,
			Role:      clipBytes(c.Role, catalogop.MaxRole),
			WorkTitle: clipBytes(c.WorkTitle, catalogop.MaxTitle),
			Year:      c.Year,
			Library:   c.LibraryName,
			Site:      site,
			HeldAt:    heldAt,
		}, heads, now)
		if errors.Is(err, catalogop.ErrMalformed) {
			// A property of the asset (a path past the op's bound, a library
			// name too long): a retry would fail the same way. Not published.
			skipped++
			log.Debug("an asset cannot be said as a hold op", "asset_id", c.AssetID, "error", err)
			continue
		}
		if err != nil {
			return written, fmt.Errorf("worker: signing hold for asset %s: %w", c.AssetID, err)
		}
		if logBytes+len(tok) > maxOpLogBytes {
			log.Warn("not publishing further holds: the catalog op-log is near the catalog-sync limit",
				"log_bytes", logBytes, "limit", maxOpLogBytes)
			break
		}
		logBytes += len(tok)
		published[catalogop.HoldKey(target, c.BlobHash)] = true
		heads = []string{catalogop.Hash(tok)}
		batch = append(batch, tok)
		if len(batch) == maxHoldBatch {
			if err := flush(); err != nil {
				return written, err
			}
		}
	}
	if err := flush(); err != nil {
		return written, err
	}
	if written > 0 || skipped > 0 {
		log.Info("published held assets for siblings to pull", "published", written, "not_expressible", skipped)
	}
	return written, nil
}

// reportedBySibling is the set of AcquireKeys whose effective acquire this
// node signed and a current member other than this node reported unsourced.
// A report on a selection since superseded counts for nothing: the newer one
// covers until it is reported in turn.
func (p PeerPull) reportedBySibling(ctx context.Context, view catalogop.View, selfKey string) (map[string]bool, error) {
	out := map[string]bool{}
	member := map[string]bool{}
	for hash, reports := range view.Unsourced {
		acq, ok := view.Accepted[hash]
		if !ok || acq.By != selfKey {
			continue
		}
		key := catalogop.AcquireKey(acq.Target, acq.Acquire.Scope, acq.Acquire.Aspect)
		if eff, ok := view.Acquisitions[key]; !ok || eff.Hash != hash {
			continue
		}
		for _, r := range reports {
			is, seen := member[r.By]
			if !seen {
				var err error
				if is, err = p.Catalog.IsSiblingKey(ctx, r.By); err != nil {
					return nil, err
				}
				member[r.By] = is
			}
			if is {
				out[key] = true
				break
			}
		}
	}
	return out, nil
}

// siteName is this node's site name as an op carries it.
func (p PeerPull) siteName() string {
	if site := clipBytes(p.Site, catalogop.MaxSite); site != "" {
		return site
	}
	return "unnamed"
}

func opLogBytes(tokens []string) int {
	n := 0
	for _, t := range tokens {
		n += len(t)
	}
	return n
}

// apply lands every effective hold op that has not settled.
func (p PeerPull) apply(ctx context.Context, view catalogop.View, selfKey string, now time.Time, log *slog.Logger) error {
	if len(view.Holds) == 0 {
		return nil
	}
	libs, err := p.Catalog.PeerPullLibraries(ctx, p.Libraries)
	if err != nil {
		return err
	}
	done, err := p.Catalog.HoldApplications(ctx)
	if err != nil {
		return err
	}
	self, err := p.Catalog.SelfPeer(ctx)
	if err != nil {
		return err
	}
	ops := make([]catalogop.Op, 0, len(view.Holds))
	for _, op := range view.Holds {
		ops = append(ops, op)
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Hash < ops[j].Hash })

	queued, changed := 0, 0
	for _, op := range ops {
		if strings.HasPrefix(op.Target.WorkKey, "music-recording:") {
			continue // retain pending until peer capability is negotiated
		}
		prior, seen := done[op.Hash]
		if seen && prior.Outcome.Final() {
			continue
		}
		app, err := p.applyHold(ctx, op, selfKey, self, libs, now, &queued, log)
		if err != nil {
			// A storage or queue failure: leave the op for the next pass
			// rather than record an outcome it did not reach.
			return fmt.Errorf("worker: applying hold op %s: %w", op.Hash, err)
		}
		app.OpHash, app.BlobHash = op.Hash, op.Hold.Blob
		if seen && prior.Outcome == app.Outcome && prior.Detail == app.Detail && prior.LibraryID == app.LibraryID {
			continue
		}
		if err := p.Catalog.RecordHoldApplication(ctx, app); err != nil {
			return err
		}
		changed++
	}
	if changed > 0 {
		log.Info("applied siblings' hold ops", "changed", changed, "pulls_queued", queued)
	}
	return nil
}

// applyHold resolves one hold op to an outcome. Errors are storage failures
// only; every expected shape is an outcome.
func (p PeerPull) applyHold(
	ctx context.Context, op catalogop.Op, selfKey, self string, libs []catalog.PeerPullLibrary,
	now time.Time, queued *int, log *slog.Logger,
) (catalog.HoldApplication, error) {
	h := op.Hold
	if op.By == selfKey {
		return catalog.HoldApplication{Outcome: catalog.HoldOwn, Detail: "signed by this peer"}, nil
	}
	from := "from site " + h.Site
	lib, why := chooseLibrary(libs, op.Target.ContentType, h.Library)
	if why != "" {
		return catalog.HoldApplication{Outcome: catalog.HoldNoLibrary, Detail: from + "; " + why}, nil
	}
	out := catalog.HoldApplication{LibraryID: lib.ID}
	if h.Size > p.MaxBlobBytes {
		out.Outcome, out.Detail = catalog.HoldTooLarge,
			fmt.Sprintf("%s; %d bytes is over this node's cap of %d", from, h.Size, p.MaxBlobBytes)
		return out, nil
	}
	inLib, err := p.Catalog.LibraryHoldsBlob(ctx, lib.ID, h.Blob)
	if err != nil {
		return out, err
	}
	if inLib {
		out.Outcome, out.Detail = catalog.HoldHeld, from+"; already in library "+lib.Name
		return out, nil
	}
	hash, err := hashing.Parse(h.Blob)
	if err != nil {
		return out, fmt.Errorf("worker: a verified hold names %q: %w", h.Blob, err)
	}
	held, err := p.Store.Has(ctx, hash)
	if err != nil {
		return out, err
	}
	if held {
		// The bytes are here and verified, by a pull or any other route: the
		// asset is recorded under the origin's keys.
		res, err := p.Adopter.Adopt(ctx, ingest.AdoptRequest{
			RootID:     lib.RootID,
			BlobHash:   h.Blob,
			RelPath:    h.Path,
			SourcePath: PeerPullSourcePath(h.Site, h.Path),
			Work: ingest.WorkOverride{
				ContentType: op.Target.ContentType,
				WorkKey:     op.Target.WorkKey,
				Title:       identification.DisplayTitle(op.Target.ContentType, h.WorkTitle),
				SortTitle:   sortTitle(op.Target.ContentType, h.WorkTitle, h.Year),
				Year:        h.Year,
			},
			EditionKey: strings.TrimPrefix(h.Scope, catalogop.ScopeEditionPrefix),
		})
		if err != nil {
			return out, err
		}
		if res.Tombstoned {
			out.Outcome, out.Detail = catalog.HoldTombstoned, from+"; the work is tombstoned here"
			return out, nil
		}
		out.Outcome, out.Detail = catalog.HoldHeld, from+"; recorded in library "+lib.Name
		return out, nil
	}

	// Not held: record the signer's claim, so replicate_blob has a row and a
	// source, and queue the pull.
	if _, err := p.Catalog.RecordOpSourcedBlob(ctx, catalog.OpSourcedBlob{
		BlobHash: h.Blob, Size: h.Size, SignerKey: op.By,
	}); err != nil {
		if errors.Is(err, catalog.ErrOpSignerNotAMember) {
			out.Outcome, out.Detail = catalog.HoldNoSiteSource, from+"; the signer is not a member of this node"
			return out, nil
		}
		return out, err
	}
	out.Outcome = catalog.HoldPulling
	failed, err := p.Catalog.LastFailedPull(ctx, h.Blob)
	if err != nil {
		return out, err
	}
	if retry := failed.Add(peerPullRetryAfter); !failed.IsZero() && now.Before(retry) {
		out.Detail = from + "; the last pull failed, retrying after " + retry.UTC().Format(time.RFC3339)
		return out, nil
	}
	if *queued >= maxPeerPullsPerPass {
		out.Detail = from + "; waiting for a pull slot"
		return out, nil
	}
	if p.Queue != nil {
		gap := replication.Gap{BlobHash: h.Blob, PeerID: self}
		if _, err := p.Queue.Enqueue(ctx, jobs.EnqueueOptions{
			Type:      replication.ReplicateBlobJobType,
			Payload:   replication.ReplicateBlobPayload{BlobHash: h.Blob, DestinationPeerID: self},
			DedupeKey: gap.DedupeKey(),
		}); err != nil {
			return out, fmt.Errorf("queueing the pull of %s: %w", h.Blob, err)
		}
		*queued++
	}
	out.Detail = from + "; pull queued"
	log.Debug("queued a pull of a sibling's held blob", "blob_hash", h.Blob, "site", h.Site, "library", lib.Name)
	return out, nil
}

// chooseLibrary picks the opted-in library a hold lands in: of the op's
// content type, preferring the origin library's name. Several of the type and
// none with the name is ambiguous, and nothing is chosen.
func chooseLibrary(libs []catalog.PeerPullLibrary, contentType, origin string) (catalog.PeerPullLibrary, string) {
	var of []catalog.PeerPullLibrary
	for _, l := range libs {
		if l.ContentType == contentType {
			of = append(of, l)
		}
	}
	var pick *catalog.PeerPullLibrary
	switch {
	case len(of) == 0:
		return catalog.PeerPullLibrary{}, "no opted-in library here has content type " + contentType
	case len(of) == 1:
		pick = &of[0]
	default:
		for i := range of {
			if of[i].Name == origin {
				pick = &of[i]
			}
		}
		if pick == nil {
			return catalog.PeerPullLibrary{}, fmt.Sprintf(
				"%d opted-in libraries here have content type %s and none is named %q", len(of), contentType, origin)
		}
	}
	if pick.RootID == "" {
		return catalog.PeerPullLibrary{}, "library " + pick.Name + " has no enabled root to record under"
	}
	return *pick, ""
}

// PeerPullSourcePath is a pulled asset's provenance: the origin site and the
// path its library root saw. A scheme no host path has, so a scan here never
// matches it, and it is never identity (§61).
func PeerPullSourcePath(site, relPath string) string { return "peer-hold:" + site + "/" + relPath }

// sortTitle is the sort title identification would give the work.
func sortTitle(contentType, title string, year int) string {
	_, st := identification.Describe(contentType, title, year)
	if st == "" {
		return strings.ToLower(title)
	}
	return st
}
