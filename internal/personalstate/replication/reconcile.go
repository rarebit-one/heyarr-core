package replication

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// ReadStore is the local side of a reconcile: the opaque spaces, wrapped keys and
// changes this node holds and fans out to Full Peers. *store.Store satisfies it;
// it is an interface so the driver is testable without a database.
type ReadStore interface {
	ListSpaces(ctx context.Context) ([]spaces.EncryptedSpace, error)
	WrappedKeysFor(ctx context.Context, spaceID string) ([]store.WrappedKey, error)
	// KeyHistory returns a space's opaque key chain, oldest epoch first
	// (ADR-0103), so the reconciler moves a rotation before the wraps it made.
	KeyHistory(ctx context.Context, spaceID string) ([]store.KeyHistoryEntry, error)
	ChangesFor(ctx context.Context, spaceID string) ([]protocol.EncryptedChange, error)
	// LatestSnapshotFor returns the newest snapshot held for a space (§44), and
	// whether one exists, so the reconciler can replicate it too. ok is false with
	// a nil error when the space has no snapshot yet.
	LatestSnapshotFor(ctx context.Context, spaceID string) (protocol.EncryptedSnapshot, bool, error)
	// Base returns the space's pinned compaction base (#681), which replicates
	// to every target with the pin, as §45's compaction metadata.
	Base(ctx context.Context, spaceID string) (protocol.EncryptedSnapshot, bool, error)
	// RecordPeerAck durably records what a target confirmed holding of a space
	// (#681): the one local write a reconcile makes, after an audit.
	RecordPeerAck(ctx context.Context, spaceID, peerID string, frontier []string, base string) error
}

// PeerLister enumerates the trusted Full Peers to replicate to. It is read FRESH
// every cycle, because the fresh read is the revocation mechanism (ADR-0012): a
// peer removed from membership simply stops appearing as a target.
type PeerLister interface {
	FullPeerTargets(ctx context.Context) ([]Target, error)
}

// An Outcome is what happened for one (peer, space): the changes pushed, or the
// error that deferred it. A deferred outcome is a recorded fact, not a failure of
// the cycle — other peers still received (ADR-0038).
type Outcome struct {
	PeerID  string
	SpaceID string
	Pushed  int
	Err     error
}

// Reconcile fans every local space out to every target, leaderless and
// idempotent. For each space it pushes the metadata and wrapped keys, offers the
// target's heads, and pushes only the changes the target is missing
// (protocol.Missing — opaque DAG reachability, no decryption). An unreachable
// target is recorded (TypeReplicationDeferred) and the reconcile moves on to the
// next; a converged space pushes nothing and still records the fact. It NEVER
// decrypts a change — every value it moves is ciphertext.
//
// Every pass of Reconcile also audits (auditSpace): it asks each target which of
// this node's changes it holds, resends the ones it lacks, and records the
// target's acknowledgement. A [Reconciler] audits on its own schedule instead.
func Reconcile(ctx context.Context, local ReadStore, pusher Pusher, targets []Target, ev *events.Log, log *slog.Logger) []Outcome {
	return reconcile(ctx, local, pusher, targets, ev, log, func(string, string) bool { return true })
}

func reconcile(ctx context.Context, local ReadStore, pusher Pusher, targets []Target, ev *events.Log, log *slog.Logger,
	auditDue func(peerID, spaceID string) bool,
) []Outcome {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	spaceList, err := local.ListSpaces(ctx)
	if err != nil {
		return []Outcome{{Err: fmt.Errorf("replication: listing local spaces: %w", err)}}
	}

	var outcomes []Outcome
	for _, t := range targets {
		for _, sp := range spaceList {
			pushed, err := replicateSpace(ctx, local, pusher, t, sp)
			if err != nil {
				// An unreachable peer is a recorded fact with a timestamp, never
				// an alarm (ADR-0038). Record it and move to the next peer — the
				// rest still converge, and the next cycle retries this one.
				log.Warn("deferred personal-state replication",
					"peer_id", t.Peer.PeerID, "space", sp.ID, "error", err)
				emit(ctx, ev, events.TypeReplicationDeferred, sp.ID,
					map[string]any{"peer_id": t.Peer.PeerID, "reason": err.Error()})
				outcomes = append(outcomes, Outcome{PeerID: t.Peer.PeerID, SpaceID: sp.ID, Err: err})
				break
			}
			// Pinning the base and auditing are snapshot trust (#681, #682). A
			// target that predates them, or a transient failure, must not undo
			// the replication that already happened: it is recorded, and the
			// target simply does not ack, so nothing is compacted against it.
			repaired, err := trustSpace(ctx, local, pusher, t, sp.ID, auditDue(t.Peer.PeerID, sp.ID))
			if err != nil {
				log.Warn("deferred personal-state snapshot trust",
					"peer_id", t.Peer.PeerID, "space", sp.ID, "error", err)
				emit(ctx, ev, events.TypeReplicationDeferred, sp.ID,
					map[string]any{"peer_id": t.Peer.PeerID, "reason": "audit: " + err.Error()})
			}
			if repaired > 0 {
				emit(ctx, ev, events.TypeStateRepaired, sp.ID,
					map[string]any{"peer_id": t.Peer.PeerID, "changes_resent": repaired})
			}
			pushed += repaired
			emit(ctx, ev, events.TypeSpaceReplicated, sp.ID,
				map[string]any{"peer_id": t.Peer.PeerID, "changes_pushed": pushed})
			outcomes = append(outcomes, Outcome{PeerID: t.Peer.PeerID, SpaceID: sp.ID, Pushed: pushed})
		}
	}
	return outcomes
}

// replicateSpace reconciles one space to one target: push its identity, its key
// history, its wrapped keys, then the changes the target is missing. Idempotent
// at every step.
//
// History goes before wraps, oldest epoch first (ADR-0103): a row moves the
// target to its epoch and drops the wraps below it, so the wraps that follow land
// at the epoch they seal, and a recipient a rotation left out loses its copy on
// the target too. A wrap the target refuses as superseded means THIS node is
// behind (a newer rotation reached the target first); the copy is skipped rather
// than failing the space, and it is never resurrected.
func replicateSpace(ctx context.Context, local ReadStore, pusher Pusher, t Target, sp spaces.EncryptedSpace) (int, error) {
	if err := pusher.PushSpace(ctx, t, sp.ID, string(sp.Kind)); err != nil {
		return 0, err
	}
	history, err := local.KeyHistory(ctx, sp.ID)
	if err != nil {
		return 0, fmt.Errorf("reading key history for %s: %w", sp.ID, err)
	}
	for _, h := range history {
		if err := pusher.PushKeyHistory(ctx, t, sp.ID, h.Epoch, h.SealedPrev); err != nil {
			return 0, err
		}
	}
	keys, err := local.WrappedKeysFor(ctx, sp.ID)
	if err != nil {
		return 0, fmt.Errorf("reading wrapped keys for %s: %w", sp.ID, err)
	}
	for _, k := range keys {
		err := pusher.PushWrappedKey(ctx, t, sp.ID, k.Recipient, k.Wrapped, k.Epoch)
		if errors.Is(err, ErrWrapSuperseded) {
			continue
		}
		if err != nil {
			return 0, err
		}
	}
	targetHeads, err := pusher.Heads(ctx, t, sp.ID)
	if err != nil {
		return 0, err
	}
	changes, err := local.ChangesFor(ctx, sp.ID)
	if err != nil {
		return 0, fmt.Errorf("reading changes for %s: %w", sp.ID, err)
	}
	// Missing answers parents first, and the loop stops at the first failure, so a
	// partial transfer never leaves the target holding a child without its parent
	// (#682): the next cycle resumes from the target's heads with nothing hidden.
	missing := protocol.Missing(changes, targetHeads)
	for _, ch := range missing {
		if err := pusher.PushChange(ctx, t, ch); err != nil {
			return 0, err
		}
	}
	// Push the base, then the latest snapshot, AFTER their changes (§44), so the
	// target holds the tail a frontier references before it receives the
	// snapshot. The base goes first: it is what compaction leaves as the only
	// recoverable copy of the compacted range (#681), so it must replicate even
	// when it is not the latest. A space with no snapshot yet skips this.
	if base, ok, err := local.Base(ctx, sp.ID); err != nil {
		return 0, fmt.Errorf("reading the compaction base for %s: %w", sp.ID, err)
	} else if ok {
		if err := pusher.PushSnapshot(ctx, t, base); err != nil {
			return 0, err
		}
	}
	if snap, ok, err := local.LatestSnapshotFor(ctx, sp.ID); err != nil {
		return 0, fmt.Errorf("reading the latest snapshot for %s: %w", sp.ID, err)
	} else if ok {
		if err := pusher.PushSnapshot(ctx, t, snap); err != nil {
			return 0, err
		}
	}
	return len(missing), nil
}

// trustSpace pins this node's compaction base on the target, then, when an audit
// is due, audits the space. It returns how many changes the audit resent.
func trustSpace(ctx context.Context, local ReadStore, pusher Pusher, t Target, spaceID string, audit bool) (int, error) {
	if base, ok, err := local.Base(ctx, spaceID); err != nil {
		return 0, fmt.Errorf("reading the compaction base for %s: %w", spaceID, err)
	} else if ok {
		// A target whose own base this one does not cover is ahead, or
		// concurrent; its audit answer still tells the truth about what it holds.
		if err := pusher.PinBase(ctx, t, spaceID, base.SnapshotID); err != nil && !errors.Is(err, ErrBaseSuperseded) {
			return 0, err
		}
	}
	if !audit {
		return 0, nil
	}
	return auditSpace(ctx, local, pusher, t, spaceID)
}

// auditSpace is the confirmed half of replication (#681, #682). The heads-based
// push above trusts the target's heads to imply their ancestry, which a target
// that lost an ancestor before #923 violates for ever: every offer marks the
// lost change known through its child. The audit instead asks the target which
// of this node's change ids it holds as rows, and counts as held, besides those,
// the causal history of the target's pinned base, walked over this node's own
// log (a compacted target holds the base in place of those rows, and resending
// them would undo its compaction). It resends everything else parents first,
// stopping at the first failure. Only when nothing is left unconfirmed does it
// record the acknowledgement compaction is proven by: every change in this
// node's log, as Heads(log), and the base the target named.
func auditSpace(ctx context.Context, local ReadStore, pusher Pusher, t Target, spaceID string) (int, error) {
	changes, err := local.ChangesFor(ctx, spaceID)
	if err != nil {
		return 0, fmt.Errorf("reading changes for %s: %w", spaceID, err)
	}
	ids := make([]string, len(changes))
	for i, c := range changes {
		ids[i] = c.ChangeID
	}
	answer, err := pusher.Held(ctx, t, spaceID, ids)
	if err != nil {
		return 0, err
	}
	confirmed := make(map[string]bool, len(changes))
	for _, id := range answer.Held {
		confirmed[id] = true
	}
	base := ""
	if answer.Base != nil {
		base = answer.Base.SnapshotID
		for id := range protocol.CausalHistory(changes, answer.Base.Frontier) {
			confirmed[id] = true
		}
	}
	var absent []protocol.EncryptedChange
	for _, c := range changes {
		if !confirmed[c.ChangeID] {
			absent = append(absent, c)
		}
	}
	resent := 0
	for _, ch := range protocol.Topological(absent) {
		if err := pusher.PushChange(ctx, t, ch); err != nil {
			return resent, err
		}
		resent++
	}
	if err := local.RecordPeerAck(ctx, spaceID, t.Peer.PeerID, protocol.Heads(changes), base); err != nil {
		return resent, fmt.Errorf("recording %s's acknowledgement of %s: %w", t.Peer.PeerID, spaceID, err)
	}
	return resent, nil
}

func emit(ctx context.Context, ev *events.Log, eventType, spaceID string, payload map[string]any) {
	if ev == nil {
		return
	}
	// A failure to record the fact must not fail the reconcile — the sync itself
	// already happened, and the next cycle re-records. Best effort.
	_, _ = ev.Emit(ctx, eventType, "encrypted_space", spaceID, payload)
}

// A Reconciler binds a local store, a pusher and the Full-Peer enumerator into
// the on-demand reconcile a caller triggers (the admin route, and a beat).
type Reconciler struct {
	local  ReadStore
	pusher Pusher
	peers  PeerLister
	events *events.Log
	log    *slog.Logger

	// auditEvery spaces audits of one (peer, space); zero audits every pass.
	auditEvery time.Duration
	now        func() time.Time
	mu         sync.Mutex
	lastAudit  map[string]time.Time
}

// NewReconciler wires a reconciler. A nil pusher or peer lister makes ReconcileAll
// a no-op that reports nothing to do, so a node with no peer surface is a
// supported state, not a wiring error.
func NewReconciler(local ReadStore, pusher Pusher, peers PeerLister, ev *events.Log, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Reconciler{local: local, pusher: pusher, peers: peers, events: ev, log: log, now: time.Now}
}

// AuditEvery spaces the audit of each (peer, space) by at least d, measured on
// clock (nil for the wall clock). The first pass after start always audits,
// which is the "once on upgrade" run (#682). Without it, every pass audits,
// which is what an on-demand reconcile wants. It returns r, for chaining.
func (r *Reconciler) AuditEvery(d time.Duration, clock func() time.Time) *Reconciler {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auditEvery = d
	if clock != nil {
		r.now = clock
	}
	return r
}

// auditDue reports whether a (peer, space) audit is due, and if so marks it
// done now: a failed audit waits for the next interval like a clean one, so an
// unreachable peer is not re-audited every pass.
func (r *Reconciler) auditDue(peerID, spaceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.auditEvery <= 0 {
		return true
	}
	key := peerID + "\x00" + spaceID
	now := r.now()
	if last, ok := r.lastAudit[key]; ok && now.Sub(last) < r.auditEvery {
		return false
	}
	if r.lastAudit == nil {
		r.lastAudit = map[string]time.Time{}
	}
	r.lastAudit[key] = now
	return true
}

// FullPeerIDs lists the trusted Full Peers this node replicates to, read fresh,
// so compaction waits for exactly those (#681). A node with no peer surface has
// none.
func (r *Reconciler) FullPeerIDs(ctx context.Context) ([]string, error) {
	if r.peers == nil {
		return nil, nil
	}
	targets, err := r.peers.FullPeerTargets(ctx)
	if err != nil {
		return nil, fmt.Errorf("replication: listing full peers: %w", err)
	}
	ids := make([]string, len(targets))
	for i, t := range targets {
		ids[i] = t.Peer.PeerID
	}
	return ids, nil
}

// ReconcileAll lists the trusted Full Peers and reconciles every local space to
// each, returning how many (peer, space) pairs converged and how many were
// deferred. It is safe to call repeatedly; a converged fleet is a no-op.
func (r *Reconciler) ReconcileAll(ctx context.Context) (replicated, deferred int, err error) {
	if r.pusher == nil || r.peers == nil {
		return 0, 0, nil
	}
	targets, err := r.peers.FullPeerTargets(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("replication: listing full peers: %w", err)
	}
	for _, o := range reconcile(ctx, r.local, r.pusher, targets, r.events, r.log, r.auditDue) {
		if o.Err != nil {
			deferred++
		} else {
			replicated++
		}
	}
	return replicated, deferred, nil
}
