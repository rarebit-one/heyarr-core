package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/media/capability"
)

// The capability advertisement beat (§6, §75, ADR-0039, M5-112).
//
// # Why this is NOT a job, when the provider health pass next door is
//
// Every other periodic sweep in this repo is enqueued by the controller and
// claimed by whichever worker is free, because invariant 4 says roles
// communicate through the job table and because deciding that work should
// happen is control-plane (§7). That shape is right whenever the answer is
// about the FLEET: any worker can check whether an indexer is reachable, and
// the answer is the same whoever asked.
//
// This answer is not. An advertisement is a statement a worker makes about
// ITSELF — about the silicon in the machine it is running on. A queued pass
// would be claimed by whichever worker happened to be free, and that worker can
// only honestly advertise about itself; the worker that was busy would go
// unrenewed and its advertisement would expire while it was perfectly healthy.
// A per-worker dedupe key does not fix it either, because nothing stops one
// worker claiming another's job.
//
// So this is a beat the worker runs about itself, and the thing that makes it
// legal under invariant 4 is unchanged: what it produces goes into the
// DATABASE, not into memory. The controller answering "which nodes can encode
// HEVC" reads rows, exactly as it reads provider health rows, and has no
// pointer to anything the worker owns. The lease heartbeat is the existing
// precedent for a per-worker loop that is not itself a job.
//
// # It EXERCISES rather than enumerates
//
// See internal/media/capability. `ffmpeg -encoders` will list a hardware AV1
// encoder on silicon that cannot encode AV1, and nothing FFmpeg prints
// distinguishes the two. So each candidate is a real encode of a handful of
// frames to a null sink, and only a successful process exit produces a
// capability.

// AdvertiseTTL is how long an advertisement stands without being renewed.
//
// It is a multiple of the beat interval rather than equal to it: at exactly one
// interval, a pass that ran ninety seconds late would expire a perfectly
// healthy worker's advertisement and route work around a node that was only
// busy. Three intervals means a worker has to miss three passes in a row —
// which is a worker that has stopped, not a worker that is slow.
//
// The other direction bounds how long a DEAD worker keeps being routed to, and
// that is the cost being paid for the tolerance above. Jobs are leased and
// re-run (ADR-0008), so the cost of routing to a dead worker is a lease timeout
// and a retry, not lost work.
const AdvertiseTTL = 3 * AdvertiseInterval

// AdvertiseInterval is how often hardware is re-verified. Declared here rather
// than in the controller so the TTL above cannot silently drift away from it.
//
// Five minutes. A pass is a handful of subprocesses encoding eight frames each
// — cheap, but not free, and it is not the sort of thing that should run every
// minute on a NAS that is also serving. What it is racing is a device being
// claimed by another process or broken by an update, and neither of those needs
// to be noticed in under a minute: the consequence of noticing late is that a
// transcode job routes to this node and fails, gets re-leased and runs
// somewhere else.
const AdvertiseInterval = 5 * time.Minute

// AdvertiserOptions configure an Advertiser.
type AdvertiserOptions struct {
	// WorkerID is this worker's identity in the advertisement — the same value
	// it leases jobs with, so an operator reading a stuck job and an operator
	// reading the fleet view are looking at the same string.
	WorkerID string
	PeerID   string
	PeerName string
	// Binary is what the startup toolchain resolved (ADR-0023). It is a VALUE
	// captured at startup and is never re-resolved here; that asymmetry with
	// the hardware half is the decision ADR-0039 records.
	Binary []capability.Held
	// Runner exercises hardware candidates. Nil means this node has no ffmpeg,
	// so there is nothing to exercise and the advertisement is the binary half
	// alone — which may itself be empty, and empty is a legitimate
	// advertisement rather than an absent one.
	Runner     capability.Runner
	Candidates []capability.Candidate
	Recorder   Advertiser
	// Interval and TTL default to AdvertiseInterval and AdvertiseTTL.
	Interval time.Duration
	TTL      time.Duration
	Clock    func() time.Time
	Logger   *slog.Logger
}

// Advertiser is the narrow slice of the catalog this needs, so the beat can be
// tested without a database and so nothing here can reach for the rest of it.
type Advertiser interface {
	AdvertiseCapabilities(ctx context.Context, ad capability.Advertisement) (capability.Change, error)
}

// CapabilityBeat re-verifies what this worker can do and records it.
type CapabilityBeat struct {
	opts AdvertiserOptions
	log  *slog.Logger
	now  func() time.Time
}

// NewCapabilityBeat builds the beat.
func NewCapabilityBeat(opts AdvertiserOptions) (*CapabilityBeat, error) {
	if opts.WorkerID == "" {
		return nil, errors.New("worker: a worker id is required to advertise capabilities")
	}
	if opts.Recorder == nil {
		return nil, errors.New("worker: an advertisement recorder is required — an advertisement " +
			"that stays in this process is a local variable, not a fleet view")
	}
	if opts.Interval <= 0 {
		opts.Interval = AdvertiseInterval
	}
	if opts.TTL <= 0 {
		opts.TTL = AdvertiseTTL
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Clock
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &CapabilityBeat{opts: opts, log: log.With("component", "capabilities"), now: now}, nil
}

// Advertise runs one pass: exercise, then record.
//
// # A narrowing is not a failure
//
// The point of the pass is to RECORD that a capability has gone. Treating that
// as an error would put the beat into a backoff at precisely the moment the
// fleet view most needs to be current, and would report the same shrinkage more
// slowly.
//
// Idempotent (invariant 9): it exercises the world and writes what it found.
// Running it twice writes the same answer the second time.
func (b *CapabilityBeat) Advertise(ctx context.Context) (capability.Change, error) {
	var probed []capability.Held
	if b.opts.Runner != nil {
		probed = capability.Probe(ctx, b.opts.Runner, b.opts.Candidates, b.now, b.log)
	}

	// Binary first, so a probe cannot overwrite a startup-resolved fact with a
	// weaker one that happens to share its name.
	ad := capability.Advertisement{
		WorkerID: b.opts.WorkerID,
		PeerID:   b.opts.PeerID,
		PeerName: b.opts.PeerName,
		Held:     capability.Merge(b.opts.Binary, probed),
		TTL:      b.opts.TTL,
	}

	change, err := b.opts.Recorder.AdvertiseCapabilities(ctx, ad)
	if err != nil {
		// Persisting IS the pass. One that exercised the hardware and then lost
		// the answer has done nothing at all.
		return capability.Change{}, err
	}

	if len(change.Lost) > 0 {
		// The half worth waking somebody for. A capability disappearing without
		// the binary changing and without a restart is a device somebody else
		// took, or a driver an update broke.
		b.log.Warn("this worker can no longer do something it could before",
			"lost", strings.Join(change.Lost, ", "),
			"still_held", strings.Join(capability.Names(ad.Held), ", "))
	}
	if len(change.Gained) > 0 {
		b.log.Info("this worker advertised new capabilities",
			"gained", strings.Join(change.Gained, ", "))
	}
	// An unchanged pass says nothing. It is the normal case, it happens every
	// few minutes forever, and a log line for it would be the only thing
	// anybody ever saw from this component.
	return change, nil
}

// Run advertises immediately and then on the beat, until ctx is cancelled.
//
// The immediate pass is the more important of the two: a restart is exactly
// when the fleet needs to know what this node can do, and waiting a whole
// interval would leave a freshly started worker invisible to routing for as
// long as the operator is most likely to be watching.
//
// A failed pass is logged and never fatal — for the same reason the
// reconciliation beat's is not. This is how the fleet NOTICES things, not how
// it works: a worker that cannot record an advertisement still claims and runs
// every job whose capability it holds, because Claim takes the capability set
// as an argument and has never consulted this table.
func (b *CapabilityBeat) Run(ctx context.Context) {
	pass := func(reason string) {
		if _, err := b.Advertise(ctx); err != nil && ctx.Err() == nil {
			b.log.Warn("could not advertise this worker's capabilities",
				"reason", reason, "error", err)
		}
	}
	pass("startup")

	ticker := time.NewTicker(b.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass("beat")
		}
	}
}

// BinaryCapabilities is what the startup toolchain contributes (ADR-0023).
//
// It exists so the advertisement records HOW each capability was established.
// The same string `ffmpeg` means something different when it came from
// resolving a path at startup than it would if something had exercised it, and
// the difference is exactly which re-verification rule applies to the row: a
// binary capability is never re-resolved, so the only thing that ever removes
// one is the advertisement expiring with the process that made it.
func BinaryCapabilities(names []string, at time.Time) []capability.Held {
	return heldFrom(names, capability.SourceBinary, "resolved at startup", at)
}

// ServiceCapabilities is what the provider registry contributes (ADR-0025).
//
// Separate from the binary half rather than lumped in with it, because they are
// different KINDS of claim and the source column is what says so: a service
// capability means "an endpoint and a credential are configured", which is a
// startup fact about this node's configuration, not about a device in it.
// Whether that service is reachable right now is provider health's answer and
// is deliberately not folded in here — a node whose indexer is down still
// advertises `search`, and its jobs still route to it and retry, which is what
// ADR-0025 decided.
func ServiceCapabilities(names []string, at time.Time) []capability.Held {
	return heldFrom(names, capability.SourceService, "configured at startup", at)
}

func heldFrom(names []string, source capability.Source, detail string, at time.Time) []capability.Held {
	out := make([]capability.Held, 0, len(names))
	for _, n := range names {
		out = append(out, capability.Held{
			Name: n, Source: source, Detail: detail, ProvedAt: at.UTC(),
		})
	}
	return out
}
