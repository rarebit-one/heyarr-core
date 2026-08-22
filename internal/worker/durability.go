package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/durability"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// lazyDurability is the gc_blobs job's half of ADR-0018's placement
// precondition, resolved on first use (M4-12).
//
// Lazy for the reason lazyPuller is: the peer private key lives at 0600 in the
// data directory and is written by the controller at first start (ADR-0010),
// the roles start concurrently and are independently runnable (ADR-0002), and a
// worker whose data directory has no key yet is an ordinary startup state
// rather than a fault. Resolving it eagerly would make that a startup failure.
//
// What it must never do is FAIL OPEN. A build error here is returned from every
// method, and each method's error is a refusal the collector reports and
// respects — not a check that quietly did not happen. That is why the zero
// value of the struct is unusable rather than permissive, and why nothing here
// returns nil on a failure to construct.
type lazyDurability struct {
	dataDir string
	peerID  string
	writer  *sql.DB
	log     *slog.Logger

	mu    sync.Mutex
	built *durability.Verifier
}

var _ integrity.Durability = (*lazyDurability)(nil)

func newLazyDurability(dataDir, peerID string, writer *sql.DB, log *slog.Logger) *lazyDurability {
	return &lazyDurability{dataDir: dataDir, peerID: peerID, writer: writer, log: log}
}

// verifier builds the real one on first use, and does not cache a failure.
//
// "The key is not there yet" resolves itself the moment the controller finishes
// starting; memoising the error would mean this worker never verified anything
// again until it was restarted, and a worker that can never verify is a worker
// that can never collect.
func (l *lazyDurability) verifier() (*durability.Verifier, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.built != nil {
		return l.built, nil
	}
	priv, err := identity.Signer(l.dataDir)
	if err != nil {
		return nil, fmt.Errorf("worker: this node cannot present a peer identity, so it cannot "+
			"establish that a blob exists on another peer, so it will not unlink one: %w", err)
	}
	material, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: priv, PeerID: l.peerID})
	if err != nil {
		return nil, fmt.Errorf("worker: %w", err)
	}
	built, err := durability.New(durability.Options{
		Material:   material,
		Controller: durability.LocalControlPlane(l.writer),
		Logger:     l.log,
	})
	if err != nil {
		return nil, err
	}
	l.built = built
	return built, nil
}

// Controller implements [integrity.Durability].
func (l *lazyDurability) Controller(ctx context.Context) error {
	v, err := l.verifier()
	if err != nil {
		return err
	}
	return v.Controller(ctx)
}

// Holds implements [integrity.Durability].
func (l *lazyDurability) Holds(ctx context.Context, p integrity.Peer, h hashing.Hash) error {
	v, err := l.verifier()
	if err != nil {
		return err
	}
	return v.Holds(ctx, p, h)
}
