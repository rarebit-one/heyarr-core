// Package peer serves and replicates bytes. Peers own bytes (spec §5).
package peer

import (
	"context"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/config"
)

// Peer is the byte-serving role.
type Peer struct {
	cfg config.Config
	log *slog.Logger
}

// New constructs the peer.
func New(cfg config.Config, log *slog.Logger) *Peer {
	return &Peer{cfg: cfg, log: log.With("role", "peer")}
}

// Name identifies the role in logs and supervision.
func (p *Peer) Name() string { return "peer" }

// Run blocks until ctx is cancelled, then stops serving.
//
// Milestone 1 fills this in: the CAS (M1-07) and range serving (M1-15). Today
// it is the wiring only.
func (p *Peer) Run(ctx context.Context) error {
	p.log.Info("peer started",
		"peer_name", p.cfg.Peer.Name,
		"site", p.cfg.Peer.Site,
		"cas_root", p.cfg.CAS.Root)
	<-ctx.Done()
	p.log.Info("peer stopped")
	return nil
}
