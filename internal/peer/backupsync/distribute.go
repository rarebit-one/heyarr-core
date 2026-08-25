package backupsync

import (
	"context"
	"log/slog"
	"sync"

	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
)

// peerPusher is the push capability [Distribute] needs. It is an interface so
// the cycle — fan-out, make-progress-with-whoever, record-the-belief — is
// testable without standing up an mTLS server; [Pusher] is the real one.
type peerPusher interface {
	PushTo(ctx context.Context, target Target, backupDir string) (int64, error)
}

// Outcome is the result of pushing to one peer in a cycle.
type Outcome struct {
	// PeerID is the peer this outcome is about.
	PeerID string
	// Generation is the generation the peer confirmed holding, or zero when the
	// push failed.
	Generation int64
	// Err is nil on success. An unreachable or refusing peer is an error here and
	// NOT a failure of the cycle: the other peers still receive.
	Err error
}

// Distribute pushes the backup in backupDir to every target concurrently,
// records the belief for each that succeeds, and returns an outcome per target
// in the same order.
//
// It never blocks on an unreachable peer (ADR-0041's progress rule, ADR-0037's
// "unreachable is ordinary"): the pushes run independently, a failure becomes
// that peer's outcome, and the others proceed. A cycle makes progress with
// whoever it has, and there is no error channel that ends it on the first
// failure — that shape is exactly what M6 learned to avoid.
func Distribute(ctx context.Context, pusher peerPusher, beliefs *Beliefs,
	targets []Target, backupDir string, clock Clock, log *slog.Logger,
) []Outcome {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	manifest, err := backup.ReadManifest(backupDir)
	if err != nil {
		log.Error("cannot read the local backup to distribute", "dir", backupDir, "error", err)
		return nil
	}

	outcomes := make([]Outcome, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			gen, err := pusher.PushTo(ctx, t, backupDir)
			outcomes[i] = Outcome{PeerID: t.Peer.PeerID, Generation: gen, Err: err}
			if err != nil {
				log.Warn("could not push a control backup to a peer; the next cycle will retry",
					"peer_id", t.Peer.PeerID, "error", err)
				return
			}
			if rerr := beliefs.Record(ctx, t.Peer.PeerID, gen, manifest.Digest, clock.Now()); rerr != nil {
				// The push landed; failing to remember it does not unland it. The
				// next cycle re-pushes (idempotent) and records again.
				log.Warn("pushed a control backup but could not record the belief",
					"peer_id", t.Peer.PeerID, "error", rerr)
			}
		}(i, t)
	}
	wg.Wait()
	return outcomes
}
