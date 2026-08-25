// Package recover rebuilds one peer's control plane and identity from a backup a
// surviving peer holds (spec §51, §82, ADR-0044, ADR-0046). Milestone 7.
//
// # What is rebuilt, and what is not
//
// Under ADR-0038 each peer is authoritative for its own site, so "recover" is
// not "recover the system" — it is rebuilding ONE peer's control plane. Two of
// §51's five inputs have no fetch path and are what this package restores: the
// peer's own control database, and its Ed25519 identity. The other three —
// content CAS, encrypted personal state, catalog snapshot — re-fill themselves
// from the fabric once the control plane is back, because content converges by
// digest and the catalog snapshot is refetchable with holding=0. Recovery is
// control-plane-first; this package does the "first", and the ordinary
// convergence cycle does the rest.
//
// # The two phases
//
// [Fetch] is the dry run: it pulls the backup from the surviving peer, verifies
// it, and reports what a restore WOULD do — which peer, which generation, how
// old, which schema, and which of §51's inputs are restored here versus
// refetched by convergence. It installs nothing.
//
// [Apply] is the destructive half: it installs the identity, restores the
// control database, binds the CAS marker, and voids the leases the dead node
// held. It refuses a data directory that is still live, because a second live
// writer for one identity is the one outcome §82 recovery must never produce.
package recover

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/peer/backupsync"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// §51's recovery inputs, and how each is provided.
const (
	// InputRestored is an input this command restores directly (the control
	// plane, the identity).
	InputRestored = "restored"
	// InputRefetched is an input the ordinary convergence cycle re-fills once the
	// control plane is back (content CAS, encrypted personal state, catalog
	// snapshot).
	InputRefetched = "refetched-by-convergence"
)

// Input is the status of one §51 recovery input.
type Input struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Plan is what a recovery would do — the dry-run report and the verified bundle
// it is about to restore.
type Plan struct {
	SourcePeerID  string    `json:"source_peer_id"`
	Generation    int64     `json:"generation"`
	TakenAt       time.Time `json:"taken_at"`
	Age           string    `json:"age"`
	SchemaVersion int64     `json:"schema_version"`
	Digest        string    `json:"digest"`
	Signed        bool      `json:"signed"`
	Omissions     []string  `json:"omissions"`
	Inputs        []Input   `json:"inputs"`

	// bundleDir is the verified bundle on disk, ready for Apply. Not serialised —
	// it is a temp path meaningful only within one run.
	bundleDir string
}

// Fetcher fetches a backup bundle from a peer. *backupsync.Pusher implements it.
type Fetcher interface {
	FetchBundle(ctx context.Context, target backupsync.Target, generation int64, destDir string) (backup.Manifest, error)
}

// FetchOptions configure the dry run.
type FetchOptions struct {
	// Fetcher pulls the bundle from the surviving peer.
	Fetcher Fetcher
	// From is the surviving peer to recover from.
	From backupsync.Target
	// Generation is which to fetch; zero fetches the latest the peer holds.
	Generation int64
	// IdentitySeed is this node's own 32-byte identity seed (operator-supplied).
	// The backup was signed by this node, so its public half verifies the
	// manifest — a recovery that trusted a backup it could not tie to its own
	// identity would restore whatever the peer chose to serve.
	IdentitySeed []byte
	// WorkDir is where the bundle is fetched to (a temp directory).
	WorkDir string
	// Now stamps the age (ADR-0017).
	Now func() time.Time
}

// ErrUnsignedRecovery is a fetched backup with no signature. A recovery trusts a
// signature over the node's own identity; an unsigned backup cannot be tied to
// it and is refused.
var ErrUnsignedRecovery = errors.New("recover: the backup is unsigned and cannot be verified against this node's identity")

// ErrSchemaTooNew is a backup at a schema version this binary does not know. A
// restore that ran the control plane at a schema it cannot fully understand
// would corrupt rather than fail, so the dry run refuses it up front — the same
// refusal sqlite.Migrate makes as a backstop, moved to where nothing is yet
// installed.
var ErrSchemaTooNew = errors.New("recover: the backup is at a newer schema than this binary knows")

// Fetch pulls the backup, verifies it against this node's own identity, and
// returns a [Plan]. It installs nothing.
func (o FetchOptions) validate() error {
	switch {
	case o.Fetcher == nil:
		return errors.New("recover: a fetcher is required")
	case len(o.IdentitySeed) != ed25519.SeedSize:
		return fmt.Errorf("recover: the identity seed must be %d bytes", ed25519.SeedSize)
	case o.WorkDir == "":
		return errors.New("recover: a work directory is required")
	}
	return nil
}

// Fetch performs the dry run. See [FetchOptions].
func Fetch(ctx context.Context, opts FetchOptions) (Plan, error) {
	if err := opts.validate(); err != nil {
		return Plan{}, err
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	manifest, err := opts.Fetcher.FetchBundle(ctx, opts.From, opts.Generation, opts.WorkDir)
	if err != nil {
		return Plan{}, err
	}
	if manifest.Signature == "" {
		return Plan{}, ErrUnsignedRecovery
	}

	// Verify the bundle against this node's OWN public key. backup.Open checks
	// the signature, the digest, integrity and the schema cross-check — the same
	// verification a received push gets, against the identity the operator
	// supplied rather than against whatever the peer asserted.
	pub := ed25519.NewKeyFromSeed(opts.IdentitySeed).Public().(ed25519.PublicKey)
	opened, err := backup.Open(ctx, opts.WorkDir, backup.OpenOptions{PublicKey: pub})
	if err != nil {
		return Plan{}, fmt.Errorf("recover: the fetched backup did not verify against this node's identity: %w", err)
	}
	_ = opened.Close()

	// Refuse a backup this binary is too old to run, in the dry run, before
	// anything is installed. sqlite.Migrate makes the same refusal as a backstop
	// during Apply, but catching it here means the operator learns it from a
	// report rather than from a half-finished restore.
	known, err := sqlite.KnownSchemaVersion()
	if err != nil {
		return Plan{}, err
	}
	if manifest.SchemaVersion > known {
		return Plan{}, fmt.Errorf("%w: backup is at %d, this binary knows up to %d",
			ErrSchemaTooNew, manifest.SchemaVersion, known)
	}

	return Plan{
		SourcePeerID:  manifest.SourcePeerID,
		Generation:    manifest.Generation,
		TakenAt:       manifest.TakenAt,
		Age:           now().UTC().Sub(manifest.TakenAt).Round(time.Second).String(),
		SchemaVersion: manifest.SchemaVersion,
		Digest:        manifest.Digest,
		Signed:        true,
		Omissions:     manifest.Omissions,
		Inputs:        inputsFor(manifest),
		bundleDir:     opts.WorkDir,
	}, nil
}

// inputsFor reports §51's five recovery inputs and how each is provided.
func inputsFor(m backup.Manifest) []Input {
	inputs := []Input{
		{Name: "control-plane-backup", Status: InputRestored, Detail: fmt.Sprintf("generation %d", m.Generation)},
		{Name: "peer-identity", Status: InputRestored, Detail: "from the supplied identity key; the node keeps its id"},
		{Name: "content-cas", Status: InputRefetched, Detail: "convergence re-pulls blobs once desired state is back"},
		{Name: "encrypted-personal-state", Status: InputRefetched, Detail: "CRDT sync re-fills it from the fabric"},
		{Name: "catalog-snapshot", Status: InputRefetched, Detail: "refetched with holding=0 from the source"},
	}
	for _, omitted := range m.Omissions {
		inputs = append(inputs, Input{Name: omitted, Status: "omitted", Detail: "not in the backup; restore from the config file (ADR-0044 Q6)"})
	}
	return inputs
}

// ApplyOptions configure the destructive restore.
type ApplyOptions struct {
	// DataDir is where the restored control plane lands.
	DataDir string
	// Store is the content store, so the CAS root marker can be bound to the
	// restored identity.
	Store *cas.FS
	// Now stamps the reclaim.
	Now func() time.Time
}

// Apply installs the identity, restores the control database, binds the CAS
// marker and voids the dead node's leases. The caller has already refused a live
// data directory (that check belongs to the command, which owns the socket).
func Apply(ctx context.Context, plan Plan, seed []byte, opts ApplyOptions) error {
	if plan.bundleDir == "" {
		return errors.New("recover: this plan has no fetched bundle; call Fetch first")
	}
	if opts.DataDir == "" {
		return errors.New("recover: a data directory is required")
	}

	// 1. The identity first, so the restored control plane and the key agree, and
	//    so a failure here happens before the database is touched. Install refuses
	//    to overwrite an existing key (ErrKeyExists) — the last guard against a
	//    second machine claiming one identity.
	if err := identity.Install(opts.DataDir, seed); err != nil {
		return err
	}

	// 2. The control database, verified again on the way in and installed at the
	//    data dir's canonical path.
	dbPath := sqlite.DataDirFor(opts.DataDir)
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if _, err := backup.Restore(ctx, plan.bundleDir, dbPath, backup.RestoreOptions{PublicKey: pub}); err != nil {
		return fmt.Errorf("recover: restoring the control database: %w", err)
	}

	// 3. Open it and run migrations. sqlite.Migrate refuses a database newer than
	//    this binary (ErrSchemaNewerThanBinary) and runs pending migrations when
	//    the binary is newer — the N-1 refusal and the N+1 upgrade, both free.
	db, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		return fmt.Errorf("recover: opening the restored database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := sqlite.Migrate(ctx, db); err != nil {
		return fmt.Errorf("recover: migrating the restored database: %w", err)
	}

	// 4. Bind the CAS marker to the restored identity, so identity.Ensure finds
	//    all three artefacts agreeing on this peer at the next start.
	if opts.Store != nil {
		if err := opts.Store.BindPeer(plan.SourcePeerID); err != nil {
			return fmt.Errorf("recover: binding the content store to the restored identity: %w", err)
		}
	}

	// 5. Void the leases the dead node held. A restored control plane must not
	//    believe jobs are running on a worker that no longer exists (§75).
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		return err
	}
	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog, Clock: clockFor(opts.Now)})
	if err != nil {
		return err
	}
	if _, err := queue.ReclaimAllLeases(ctx); err != nil {
		return fmt.Errorf("recover: voiding the previous node's job leases: %w", err)
	}

	// 6. Record the restoration in the restored plane itself (invariant 7).
	_, _ = eventLog.Emit(ctx, backup.EventRestored, "restore", plan.SourcePeerID, map[string]any{
		"source_generation": plan.Generation,
		"source_taken_at":   plan.TakenAt,
		"from_peer":         "recover --from-peer",
	})
	return nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func clockFor(now func() time.Time) jobs.Clock {
	if now == nil {
		return systemClock{}
	}
	return funcClock(now)
}

type funcClock func() time.Time

func (f funcClock) Now() time.Time { return f() }
