package replication

import (
	"context"
	"fmt"
	"log/slog"

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
	ChangesFor(ctx context.Context, spaceID string) ([]protocol.EncryptedChange, error)
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
func Reconcile(ctx context.Context, local ReadStore, pusher Pusher, targets []Target, ev *events.Log, log *slog.Logger) []Outcome {
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
			emit(ctx, ev, events.TypeSpaceReplicated, sp.ID,
				map[string]any{"peer_id": t.Peer.PeerID, "changes_pushed": pushed})
			outcomes = append(outcomes, Outcome{PeerID: t.Peer.PeerID, SpaceID: sp.ID, Pushed: pushed})
		}
	}
	return outcomes
}

// replicateSpace reconciles one space to one target: push its identity, its
// wrapped keys, then the changes the target is missing. Idempotent at every step.
func replicateSpace(ctx context.Context, local ReadStore, pusher Pusher, t Target, sp spaces.EncryptedSpace) (int, error) {
	if err := pusher.PushSpace(ctx, t, sp.ID, string(sp.Kind)); err != nil {
		return 0, err
	}
	keys, err := local.WrappedKeysFor(ctx, sp.ID)
	if err != nil {
		return 0, fmt.Errorf("reading wrapped keys for %s: %w", sp.ID, err)
	}
	for _, k := range keys {
		if err := pusher.PushWrappedKey(ctx, t, sp.ID, k.Recipient, k.Wrapped); err != nil {
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
	missing := protocol.Missing(changes, targetHeads)
	for _, ch := range missing {
		if err := pusher.PushChange(ctx, t, ch); err != nil {
			return 0, err
		}
	}
	return len(missing), nil
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
}

// NewReconciler wires a reconciler. A nil pusher or peer lister makes ReconcileAll
// a no-op that reports nothing to do, so a node with no peer surface is a
// supported state, not a wiring error.
func NewReconciler(local ReadStore, pusher Pusher, peers PeerLister, ev *events.Log, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Reconciler{local: local, pusher: pusher, peers: peers, events: ev, log: log}
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
	for _, o := range Reconcile(ctx, r.local, r.pusher, targets, r.events, r.log) {
		if o.Err != nil {
			deferred++
		} else {
			replicated++
		}
	}
	return replicated, deferred, nil
}
