// Package events is the append-only event log and its fan-out (spec §76).
//
// Events ship in Milestone 1 deliberately (ADR-0009). §61 names "polling as the
// only integration model" as an *arr failure, but the practical argument is
// narrower: retrofitting events means auditing every mutation site, and that
// audit gets more expensive with every milestone.
//
// # No per-blob events during replication
//
// Invariant 7 says every state transition emits an event. It does not say every
// blob emits an event, and in the peer plane the difference decides whether the
// log is readable at all.
//
// A first full sync with a peer holding a hundred thousand blobs moves through
// inventory exchange, reconciliation and transfer. If each of those emitted per
// blob, one ordinary onboarding would write hundreds of thousands of events —
// events no operator reads, which bury the handful of transitions anyone
// actually watches for, and which a slow SSE subscriber is then dropped for
// failing to keep up with (ADR-0009). The same argument already keeps
// blob.verified and quality_profile.evaluated out of this package: a fact that
// is true of every item is state, and state belongs in a table.
//
// So the peer plane draws the line at work, not at items:
//
//   - Inventory reporting emits once per report CYCLE, with counts
//     (sync.inventory_reported). The per-blob facts live in the replicas table.
//   - Reconciliation emits once per reconciliation CYCLE, with counts
//     (sync.reconciled) — not once per job it enqueued, which job.enqueued
//     already reports.
//   - Transfers emit per transfer (replication.transfer_changed), because a
//     transfer is a discrete unit of work with a start and a terminal outcome.
//     That is the only per-blob event in the peer plane, and it is bounded by
//     the transfer queue an operator throttles rather than by the size of the
//     library.
//
// Before adding a replication event, ask whether it fires once per cycle or
// once per blob. If it is once per blob and it is not a transfer, the fact
// belongs in a table and the transition belongs in the payload of a cycle
// event that already exists.
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Event namespaces from §76. Milestone 1 uses this subset.
const (
	TypeBlobCreated = "blob.created"
	// TypeBlobReclaimed is a garbage collection sweep freeing bytes (ADR-0018).
	// There is deliberately no matching "blob.verified": a deep fsck over a
	// healthy library would emit one per blob, which is a hundred thousand
	// events recording that nothing happened. A successful verification is
	// recorded as replicas.verified_at, which is state rather than a transition.
	TypeBlobReclaimed  = "blob.reclaimed"
	TypeReplicaPresent = "replica.present"
	TypeReplicaCorrupt = "replica.corrupt"
	// TypeReplicaMissing is a blob the catalog knows about whose bytes are not
	// on this peer at all. Distinct from corrupt: corrupt means we still hold
	// evidence and it is quarantined, missing means we hold nothing.
	TypeReplicaMissing = "replica.missing"
	// TypeBlobProbed is ffprobe having described a blob's bytes (§29). It is
	// under blob.* rather than content.* because a probe describes BYTES —
	// two assets sharing a blob share one probe, and there is no asset in the
	// subject at all.
	TypeBlobProbed      = "blob.probed"
	TypeIngestCompleted = "ingest.completed"
	TypeAssetCreated    = "content.asset.created"
	// TypeWorkCreated is a Work appearing in the catalog, however it got there
	// — scanned, or created because somebody wanted it (M3-02). It is under
	// content.* rather than desired.* deliberately: a Work appearing is a fact
	// about the catalog, and a subscriber watching the catalog grow should see
	// it whichever path created it. The payload says which.
	TypeWorkCreated      = "content.work.created"
	TypeLibraryCreated   = "content.library.created"
	TypeLibraryRootAdded = "content.library_root.added"
	TypeAssetMissing     = "content.asset.missing"
	TypeAssetDeleted     = "content.asset.deleted"
	// #nosec G101 -- an event type name, not a credential
	TypeTokenCreated = "system.token.created"
	TypeTokenRevoked = "system.token.revoked"
	TypeJobEnqueued  = "job.enqueued"
	TypeJobSucceeded = "job.succeeded"
	TypeJobFailed    = "job.failed"
	TypeScanProgress = "system.scan.progress"
	// TypeWorkerCapabilitiesChanged is a worker's advertisement of what it can
	// do having become different from what it advertised last time (ADR-0039,
	// M5-112). It is under system.* because it describes a NODE's abilities
	// rather than any content, and it names the CHANGE rather than either
	// direction: there is deliberately no capability.gained and no
	// capability.lost, because two types would be two places to forget to emit
	// and the interesting half — losing a hardware encoder without the binary
	// changing and without a restart — is the half that would get forgotten.
	// The payload carries `gained` and `lost`, so a subscriber can filter on
	// either. Emitted once per advertisement that CHANGED something, never per
	// capability and never on a beat that found the world unaltered; a fleet
	// re-verifying every few minutes would otherwise write a steady stream of
	// events saying nothing happened.
	TypeWorkerCapabilitiesChanged = "system.worker.capabilities_changed"
	TypeSystemStarted             = "system.started"
	TypeSystemStopped             = "system.stopped"
	TypePeerRegistered            = "peer.registered"
	// TypePeerIdentityEstablished is this node's Ed25519 keypair being
	// generated and recorded for the first time (§26, ADR-0012). It is a
	// distinct transition from peer.registered: the row can exist for a while
	// before it has an identity, and "when did this peer become able to
	// authenticate?" is the question asked when two peers turn out to claim
	// one identity. Emitted once per peer, ever — a second one means a key was
	// regenerated, which is a re-enrolment and should be loud.
	TypePeerIdentityEstablished = "peer.identity_established"

	// identity.* is Milestone 8 device identity (§40, ADR-0048): a user
	// identity being pinned, a device the user vouches for being enrolled, and
	// either being revoked. They are the ADR-0012 peer-membership transitions
	// applied to users rather than peers, so they read the same way in the log:
	// "who became able to authenticate here, and when?".
	TypeUserEnrolled   = "identity.user.enrolled"
	TypeUserRevoked    = "identity.user.revoked"
	TypeDeviceEnrolled = "identity.device.enrolled"
	TypeDeviceRevoked  = "identity.device.revoked"

	// lease.* is the Milestone 8 cross-site access lease (§54, ADR-0048, #285):
	// a signed, expiring grant this peer issued, and its revocation. These are
	// durable STATE transitions (invariant 7). Honouring and refusing a lease
	// are per-request and belong in metrics, not the log — this package's
	// standing rule is against one event per item.
	TypeLeaseIssued  = "lease.issued"
	TypeLeaseRevoked = "lease.revoked"

	// personalstate.* is the Milestone 9 encrypted personal-state plane (§38,
	// §41, §79, ADR-0049). They are METADATA transitions only — a space came into
	// existence, a wrapped copy of its key was stored for a recipient — and carry
	// NO plaintext: the peer that emits them holds ciphertext it cannot read, so
	// the log records that a space exists and which keys can read it, never a name
	// or a key (§38). Invariant 7 still applies: the plane's state transitions are
	// events like every other, they are just opaque ones.
	TypeSpaceCreated    = "personalstate.space.created"
	TypeSpaceKeyWrapped = "personalstate.space.key_wrapped"
	// TypeSpaceKeyRevoked is a wrapped copy of a space key being deleted for a
	// recipient — the storage side of device revocation (§41, ADR-0022). Opaque:
	// it records that a recipient can no longer read the space, never a key.
	TypeSpaceKeyRevoked = "personalstate.space.key_revoked"
	// TypeChangeStored is a peer accepting an encrypted CRDT change into a space
	// (§42, §44). Opaque like its siblings: it records that a change with this id
	// landed, never the plaintext the peer cannot read.
	TypeChangeStored = "personalstate.change.stored"

	// desired.* is §76's own category for wanting (M3-02).
	//
	// TypeDesiredSatisfied predates them: it was declared in Milestone 1 and
	// emitted by NOTHING until now. It stays reserved here rather than being
	// emitted by this issue, because being wanted and being satisfied are
	// different transitions and M3-05 owns the second one.
	// acquisition.* is §76's own category, and every edge of §64's machine
	// emits one (invariant 7, M3-03).
	//
	// There is ONE event type for a phase change rather than one per edge. The
	// payload carries the transition, the phase before and the phase after, so
	// a subscriber can filter on any of them — and thirteen event types would
	// be thirteen places to forget to emit, which is exactly what invariant 7
	// exists to prevent.
	//
	// The satisfaction axes get their own type because they move on a
	// different schedule and for a different reason: reconciliation, not the
	// pipeline. A quality profile edit can unsatisfy a want that nothing else
	// touched (§57), and a subscriber watching "what changed about what I
	// have" should not have to filter that out of the pipeline stream.
	TypeAcquisitionPhaseChanged = "acquisition.phase_changed"
	TypeAcquisitionSatisfaction = "acquisition.satisfaction_changed"

	// The upgrade workflow (§60, M3-06).
	//
	// TypeUpgradeFound is a strictly better release being available for
	// something already satisfied. It is emitted when the upgrade is DECIDED,
	// not when the scan notices eligibility — a beat that re-announced the
	// same available upgrade every five minutes would be a heartbeat rather
	// than an event stream.
	//
	// TypeUpgradeSuperseded is the incumbent being logically deleted once the
	// replacement is under management (ADR-0018). Its payload says
	// bytes_removed explicitly, because that is the whole point of ADR-0018
	// and the first question anyone reading the log will have.
	//
	// There is no "upgrade.completed": an upgrade completing IS the
	// replacement reaching AVAILABLE, which acquisition.phase_changed already
	// reports, followed by the supersession below. A third event would say a
	// third time what two already said.
	TypeUpgradeFound      = "acquisition.upgrade_found"
	TypeUpgradeSuperseded = "acquisition.upgrade_superseded"

	// The search job (§60, §63, M3-12).
	//
	// TypeSearchCompleted reports what a search FOUND and what was done with
	// it — how many candidates, how many acceptable, and which was selected.
	// It is one event per search rather than one per candidate: a search that
	// found twelve releases would otherwise emit twelve times to say one
	// thing, and the twelve explanations are durable in release_candidates,
	// which is where something wanting the detail should look.
	//
	// It is emitted for an EMPTY search too. "We looked and found nothing" is
	// the outcome an operator most needs to see — a want that goes quiet with
	// no record is the failure mode §60 keeps rejection reasons to avoid — and
	// it is the one case that leaves no candidate rows behind to explain
	// itself.
	TypeSearchCompleted = "acquisition.search_completed"

	// TypeCandidateOverridden is a PERSON choosing a candidate against the
	// scorer's ranking (§60's manual override).
	//
	// Its own type rather than a flag on the phase change, because it answers
	// a different question. acquisition.phase_changed says the want reached
	// SELECTED; this says a human disagreed with the deterministic scorer and
	// what the scorer had said instead. Something auditing "where did we
	// depart from policy" should be able to subscribe to exactly that, without
	// filtering every selection the machine made on its own.
	TypeCandidateOverridden = "acquisition.candidate_overridden"

	// TypeReleaseBlocked is a release a want will not choose again (M3-13).
	//
	// Under acquisition.* rather than desired.* because it is a fact about the
	// PIPELINE — what it will and will not select — rather than about what the
	// operator wants, which has not changed.
	//
	// It matters to a subscriber more than its size suggests: without it, a
	// want that keeps failing and a want that has stopped choosing the thing
	// that keeps failing look identical from outside.
	TypeReleaseBlocked = "acquisition.release_blocked"

	TypeDesiredCreated   = "desired.created"
	TypeDesiredUpdated   = "desired.updated"
	TypeDesiredRemoved   = "desired.removed"
	TypeDesiredSatisfied = "desired.satisfied"

	// Quality profiles are POLICY, and policy.* is a category §76 does not
	// list (M3-01).
	//
	// §76 says "categories include", so the list is open, and none of the
	// listed ones fits. A profile is not content, not a job, not playback, and
	// not desire: it is the standard desire is measured against. The nearest
	// candidate was desired.*, and overloading it would mean a subscriber
	// filtering desired.* for "what does the operator want" also receives
	// profile CRUD, which is a different question.
	//
	// It matters more than it looks: editing a profile can retroactively
	// unsatisfy a DesiredItem that nothing else touched, so this is a
	// transition an operator genuinely needs to see in the stream (§57).
	//
	// There is no "quality_profile.evaluated" event and there must not be. An
	// evaluation happens per candidate per search, and recording each one
	// would put thousands of events in the log to say that arithmetic
	// happened. §63's inspectability is served by persisting the evaluation
	// (M3-12), which is state, not by emitting it, which would be noise.
	TypeQualityProfileCreated = "policy.quality_profile.created"
	TypeQualityProfileUpdated = "policy.quality_profile.updated"
	TypeQualityProfileDeleted = "policy.quality_profile.deleted"

	// Device registration lives under playback.* rather than a namespace of
	// its own: §76 enumerates the categories, a device is not content, and a
	// device exists in this system for exactly one reason. A client following
	// playback.* wants to know a new television appeared.
	//
	// There is no "device.seen" event. A device re-registering with an
	// unchanged profile is not a state transition, and emitting for it would
	// turn every app launch in the house into an event — an event stream that
	// is mostly noise is one nobody follows (M2-05).
	TypeDeviceRegistered  = "playback.device.registered"
	TypeDeviceUpdated     = "playback.device.updated"
	TypePrivateStateHeads = "private_state.heads"

	// The peer plane (§76, Milestone 4). Reserved in one change, before the
	// emitters land, because six issues need event types at once and six
	// independent additions to this block would be six conflicting edits and
	// six naming conventions. Each type below is claimed by the issue named in
	// its comment; nothing here is emitted yet.
	//
	// peer.registered already exists above. Milestone 4 extends it rather than
	// replacing it: it is emitted today only for the self-peer that ADR-0010
	// creates at startup, and M4-04 gives it a second, non-self subject. A
	// separate "peer.joined" would mean a subscriber asking "what peers does
	// this system know about" had to watch two types to learn the same fact.
	//
	// replica.present, replica.corrupt and replica.missing also already exist,
	// and Milestone 4 adds no replica type. What changes is the SUBJECT: until
	// now every replica event described this peer's own copy, and M4-07/M4-09
	// emit them for a remote peer's copy for the first time. The transition
	// being reported is identical, so the type is identical and the payload
	// says which peer.

	// TypePeerRemoved is membership revoked — a peer this system will no
	// longer talk to, replicate to, or count as holding a replica (M4-04).
	//
	// Its own type rather than a peer.health_changed transition to some
	// "removed" state, because they answer different questions and decay
	// differently. Health is a fact about reachability that flaps; removal is
	// a fact about membership that a human decided and that does not flap. A
	// subscriber reconciling "who is in this fabric" wants exactly this, not
	// the health stream with one terminal value filtered out of it.
	TypePeerRemoved = "peer.removed"

	// TypePeerHealthChanged is a peer crossing between reachable and
	// unreachable, edge-triggered, with the transition in the payload (M4-10).
	//
	// There is deliberately no peer.up / peer.down pair. Two types for two
	// edges of one machine is the shape invariant 7 keeps failing on: it is
	// two places to forget to emit, and it forces a subscriber that cares
	// about reachability at all to subscribe to both and reassemble the
	// machine itself. The payload carries the state before and after, so
	// filtering for "went down" stays possible without a second type.
	//
	// Edge-triggered is the load-bearing word: it is emitted when health
	// CHANGES, never on each successful probe. A health check that ran every
	// thirty seconds and emitted each time would be a heartbeat, and a
	// heartbeat in the event log is the same mistake as blob.verified above —
	// a hundred thousand events recording that nothing happened.
	TypePeerHealthChanged = "peer.health_changed"

	// The transitions a peer.registered payload may carry (M4-04).
	//
	// They live here, beside the type whose payload carries them, rather than
	// in the package that emits them. Two emitters already exist — the self
	// peer this node creates at startup (ADR-0010) and the operator enrolling
	// another site — and a vocabulary defined next to one of them is a
	// vocabulary the other one spells slightly differently. A subscriber
	// filtering on "enrolled" should not have to know which code path wrote
	// the row.
	//
	// One type with a transition rather than peer.registered plus
	// peer.endpoint_changed, for the reason acquisition.phase_changed gives
	// above: N types are N places to forget to emit, and a subscriber
	// reconciling "who is in this fabric and where" would have to watch all of
	// them to learn one fact.
	PeerTransitionEnrolled = "enrolled"
	// PeerTransitionEndpointChanged is a member reachable somewhere else. Its
	// identity is unchanged: a peer is its public key, not its address
	// (ADR-0012).
	PeerTransitionEndpointChanged = "endpoint_changed"
	// PeerTransitionRemoved is membership revoked, carried in peer.removed.
	PeerTransitionRemoved = "removed"

	// TypeReplicationTransferChanged is one blob transfer between two peers
	// moving through its lifecycle: started, succeeded or failed, with the
	// transition and both peers in the payload (M4-09).
	//
	// ONE type for the whole machine, not one per edge, for the reason
	// acquisition.phase_changed gives above: N event types would be N places
	// to forget to emit. A subscriber wanting only failures filters the
	// payload, which it must be able to do anyway.
	//
	// This is the one type in this namespace that is per-blob, and it is the
	// boundary the rule below is drawn around: a transfer is work actually
	// being done to a specific blob, on a queue an operator throttles and
	// watches. Nothing else in replication may be per-blob — see "No per-blob
	// events during replication" in the package doc.
	TypeReplicationTransferChanged = "replication.transfer_changed"

	// TypeSyncInventoryReported is a peer having reported what it holds — one
	// event per report CYCLE, carrying counts and the peer, not one per blob
	// in the inventory (M4-07).
	//
	// A first inventory exchange with a peer holding a hundred thousand blobs
	// would otherwise put a hundred thousand events in the log to say one
	// thing: "we now know what that peer has". The per-blob facts are durable
	// in the replicas table, which is state and is where anything wanting the
	// detail should look — exactly the argument that keeps blob.verified and
	// quality_profile.evaluated out of this file.
	TypeSyncInventoryReported = "sync.inventory_reported"

	// TypeSyncReconciled is one reconciliation cycle finishing, carrying what
	// it decided: how many blobs were under-replicated, how many transfers it
	// enqueued, how many it could not place (M4-08).
	//
	// Not one event per enqueued job — job.enqueued already reports that, per
	// job, and repeating it here would say the same thing twice in two
	// vocabularies. This type exists for the fact job.enqueued cannot express:
	// that a full pass ran, and what the fabric looked like when it did. A
	// cycle that decided to do NOTHING still emits, because "we looked and
	// everything is sufficiently replicated" is the outcome an operator most
	// needs and the one that leaves no job rows behind to prove it happened.
	TypeSyncReconciled = "sync.reconciled"

	// TypeCatalogSnapshotBuilt is one catalog snapshot being built for
	// transfer to a peer (M4-13) — one event per build.
	//
	// There is no matching catalog.snapshot_applied here: applying a snapshot
	// changes the catalog, and those changes already emit content.* events on
	// the receiving peer. A subscriber watching the catalog grow should not
	// have to know whether a Work arrived by scan, by want, or by snapshot;
	// content.work.created says so in its payload, as its comment above
	// promises.
	TypeCatalogSnapshotBuilt = "catalog.snapshot_built"
)

const timeFormat = time.RFC3339Nano

// Event is one recorded state transition.
type Event struct {
	Seq         int64           `json:"seq"`
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	SubjectType string          `json:"subject_type,omitempty"`
	SubjectID   string          `json:"subject_id,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Clock is injected so ordering assertions do not depend on wall time.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Log appends events durably and fans them out to live subscribers.
//
// Durability first, then fan-out: a subscriber must never see an event that is
// not in the log, because it may act on it and then the log is the record of
// what happened.
type Log struct {
	writer *sql.DB
	reader *sql.DB
	clock  Clock
	log    *slog.Logger

	mu     sync.RWMutex
	subs   map[int64]*Subscription
	nextID int64
}

// Options configure a Log.
type Options struct {
	Writer *sql.DB
	Reader *sql.DB
	Clock  Clock
	Logger *slog.Logger
}

// New constructs a Log.
func New(opts Options) (*Log, error) {
	if opts.Writer == nil {
		return nil, errors.New("events: a writer database is required")
	}
	reader := opts.Reader
	if reader == nil {
		reader = opts.Writer
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Log{
		writer: opts.Writer,
		reader: reader,
		clock:  clock,
		log:    logger,
		subs:   map[int64]*Subscription{},
	}, nil
}

// rowQuerier is the one method appending an event needs, so the same code can
// run on the writer pool or inside a caller's transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Emit appends an event durably and then publishes it to live subscribers.
//
// It writes on the writer pool, which holds exactly one connection (ADR-0003).
// Calling it from inside a transaction started on that same pool would
// therefore wait for a connection the caller is holding, and block until the
// context expires. Emit from inside a transaction with EmitTx.
func (l *Log) Emit(ctx context.Context, eventType, subjectType, subjectID string, payload any) (Event, error) {
	e, err := l.append(ctx, l.writer, eventType, subjectType, subjectID, payload)
	if err != nil {
		return Event{}, err
	}
	l.publish(e)
	return e, nil
}

// EmitTx appends an event inside the caller's transaction and does NOT publish
// it. Publish the returned events with Publish once the transaction commits.
//
// Two things make this the only correct way to record a state transition that
// is part of a larger write:
//
//   - The writer pool is a single connection (ADR-0003). An Emit nested inside
//     an InTx on that pool waits for a connection its own caller is holding,
//     and the symptom is a hang until the context expires rather than an error.
//   - A subscriber must never see an event whose transaction later rolls back.
//     It may act on it, and the log is the record of what happened (§76).
//
// The database still assigns seq, so ordering remains the database's job.
func (l *Log) EmitTx(ctx context.Context, tx *sql.Tx, eventType, subjectType, subjectID string, payload any) (Event, error) {
	if tx == nil {
		return Event{}, errors.New("events: a transaction is required — use Emit outside one")
	}
	return l.append(ctx, tx, eventType, subjectType, subjectID, payload)
}

// Publish fans out events that are already durable. It is the second half of
// EmitTx and must be called only after the transaction has committed.
func (l *Log) Publish(evs ...Event) {
	for _, e := range evs {
		l.publish(e)
	}
}

func (l *Log) append(ctx context.Context, q rowQuerier, eventType, subjectType, subjectID string, payload any) (Event, error) {
	if eventType == "" {
		return Event{}, errors.New("events: type must be set")
	}
	encoded := json.RawMessage("{}")
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Event{}, fmt.Errorf("events: encoding %s payload: %w", eventType, err)
		}
		encoded = b
	}

	e := Event{
		ID:          uuid.Must(uuid.NewV7()).String(),
		Type:        eventType,
		SubjectType: subjectType,
		SubjectID:   subjectID,
		Payload:     encoded,
		CreatedAt:   l.clock.Now(),
	}

	// The database assigns seq, so ordering is the database's job rather than a
	// counter two writers could race on.
	row := q.QueryRowContext(ctx, `
		INSERT INTO events (id, type, subject_type, subject_id, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING seq`,
		e.ID, e.Type, e.SubjectType, e.SubjectID, string(e.Payload), e.CreatedAt.Format(timeFormat))
	if err := row.Scan(&e.Seq); err != nil {
		return Event{}, fmt.Errorf("events: appending %s: %w", eventType, err)
	}
	return e, nil
}

// publish delivers to live subscribers. It never blocks the caller: a slow
// subscriber is dropped rather than backpressured, because the alternative is
// one stalled SSE client wedging every write in the system.
func (l *Log) publish(e Event) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, sub := range l.subs {
		if !sub.matches(e.Type) {
			continue
		}
		select {
		case sub.ch <- e:
		default:
			sub.markDropped()
			l.log.Warn("event subscriber is too slow; dropping",
				"subscriber", sub.id, "type", e.Type, "seq", e.Seq)
		}
	}
}

// Since returns events after seq, oldest first, up to limit.
//
// This is what makes reconnection gapless: a client that saw seq N asks for
// everything after N, then switches to the live stream.
func (l *Log) Since(ctx context.Context, after int64, types []string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	query := `SELECT seq, id, type, subject_type, subject_id, payload, created_at
		FROM events WHERE seq > ?`
	args := []any{after}
	if len(types) > 0 {
		clause, typeArgs := typeFilter(types)
		if clause != "" {
			// #nosec G202 -- clause is assembled only from the literals
			// "type LIKE ?" and "type = ?"; every caller-supplied value goes
			// through a bind parameter in typeArgs. TestTypeFilterOnlyEmitsBoundClauses
			// pins that, so this stays true if typeFilter changes.
			query += " AND (" + clause + ")"
			args = append(args, typeArgs...)
		}
	}
	query += " ORDER BY seq ASC LIMIT ?"
	args = append(args, limit)

	rows, err := l.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("events: reading since %d: %w", after, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Event
	for rows.Next() {
		var e Event
		var payload, createdAt string
		if err := rows.Scan(&e.Seq, &e.ID, &e.Type, &e.SubjectType, &e.SubjectID, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("events: reading since %d: %w", after, err)
		}
		e.Payload = json.RawMessage(payload)
		if t, err := time.Parse(timeFormat, createdAt); err == nil {
			e.CreatedAt = t
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: reading since %d: %w", after, err)
	}
	return out, nil
}

// Latest returns the highest sequence number recorded, or 0 for an empty log.
func (l *Log) Latest(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	if err := l.reader.QueryRowContext(ctx, `SELECT max(seq) FROM events`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("events: reading the latest sequence: %w", err)
	}
	return seq.Int64, nil
}

// Subscription is a live feed of events.
type Subscription struct {
	id      int64
	ch      chan Event
	types   []string
	log     *Log
	once    sync.Once
	dropped atomic64
}

type atomic64 struct {
	mu sync.Mutex
	n  int64
}

func (a *atomic64) add() {
	a.mu.Lock()
	a.n++
	a.mu.Unlock()
}

func (a *atomic64) load() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

// Subscribe returns a live feed. Types may contain exact names or prefix
// patterns ending in `*`, matching §76's namespaces.
//
// The buffer is bounded on purpose. An unbounded queue turns a stalled client
// into unbounded memory growth, which fails the whole process instead of the
// one connection that is actually broken.
func (l *Log) Subscribe(buffer int, types ...string) *Subscription {
	if buffer <= 0 {
		buffer = 256
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	sub := &Subscription{
		id:    l.nextID,
		ch:    make(chan Event, buffer),
		types: types,
		log:   l,
	}
	l.subs[sub.id] = sub
	return sub
}

// Events is the channel to range over.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Dropped reports how many events this subscriber was too slow to take. A
// non-zero count means the client's view has gaps and it should reconnect with
// ?after= rather than trusting the stream.
func (s *Subscription) Dropped() int64 { return s.dropped.load() }

// Close unsubscribes. Safe to call more than once.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.log.mu.Lock()
		delete(s.log.subs, s.id)
		s.log.mu.Unlock()
		close(s.ch)
	})
}

func (s *Subscription) markDropped() { s.dropped.add() }

func (s *Subscription) matches(eventType string) bool {
	if len(s.types) == 0 {
		return true
	}
	for _, pattern := range s.types {
		if matchType(pattern, eventType) {
			return true
		}
	}
	return false
}

// matchType supports exact names and trailing-* prefixes, which is what §76's
// namespaces (`blob.*`, `job.*`) need and nothing more.
func matchType(pattern, eventType string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(eventType, prefix)
	}
	return pattern == eventType
}

// SubscriberCount reports how many live subscriptions exist.
func (l *Log) SubscriberCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.subs)
}

func typeFilter(types []string) (string, []any) {
	var clauses []string
	var args []any
	for _, t := range types {
		if t == "" || t == "*" {
			return "", nil
		}
		if prefix, ok := strings.CutSuffix(t, "*"); ok {
			clauses = append(clauses, "type LIKE ?")
			args = append(args, prefix+"%")
			continue
		}
		clauses = append(clauses, "type = ?")
		args = append(args, t)
	}
	return strings.Join(clauses, " OR "), args
}
