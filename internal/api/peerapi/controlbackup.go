package peerapi

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"net/http"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// ControlBackupPath is where a peer pushes, and asks about, control-plane
// backups (§50, M7-03, ADR-0046). Exported so the pushing side spells it once.
const ControlBackupPath = Prefix + "/control-backup"

// ControlBackupManifestHeader carries the base64-encoded JSON manifest of a
// pushed backup. The manifest is small and self-describing; the request body is
// the snapshot bytes, so the two are not muddled into one stream.
const ControlBackupManifestHeader = "X-Heyarr-Control-Backup"

// maxControlBackupBody bounds a pushed snapshot. A control plane is small — a
// homelab's desired state, policy and membership — so this is generous, not a
// target, and its job is to stop a peer streaming forever.
const maxControlBackupBody = 512 << 20

// ControlBackupSink receives control-plane backups pushed to this peer and
// reports what it holds (§50). Optional, and 503 when nil, like the other peer
// sinks — the OpenAPI parity test walks the router, so the routes are mounted
// whether or not a store is behind them.
//
// It is primitive on purpose: this package does not import persistence, so the
// manifest crosses as raw bytes and the caller (a peer) is identified by the
// certificate, never by anything in the body.
type ControlBackupSink interface {
	// ReceiveBackup verifies and stores a backup pushed by sourcePeerID, whose
	// pinned key is sourceKey, and returns the generation now held. A backup
	// that fails verification is an error and nothing is stored.
	ReceiveBackup(ctx context.Context, sourcePeerID string, sourceKey ed25519.PublicKey,
		manifestJSON []byte, snapshot io.Reader) (generation int64, err error)
	// HeldBackups lists the generations held for a source, newest first.
	HeldBackups(sourcePeerID string) (generations []int64, err error)
}

// heldResponse is what GET /peer/v1/control-backup answers: what this peer holds
// of the CALLING peer's control plane.
type heldResponse struct {
	Source      string  `json:"source"`
	Generations []int64 `json:"generations"`
	Latest      int64   `json:"latest"`
}

// receivedResponse is what a successful push is answered with.
type receivedResponse struct {
	Generation int64 `json:"generation"`
}

// handleControlBackupReceive stores a backup a peer pushed of its OWN control
// plane. The source is the certificate's peer, never anything in the body: a
// peer may push only what it can sign as itself, and the store re-checks that
// the manifest's source matches (ADR-0044 Q2, ADR-0046).
func (s *Server) handleControlBackupReceive(w http.ResponseWriter, r *http.Request) {
	peer, ok := PeerFrom(r.Context())
	if !ok {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.controlBackup == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"No Control-Plane Store", "this node does not hold control-plane backups for peers"))
		return
	}

	header := r.Header.Get(ControlBackupManifestHeader)
	if header == "" {
		httpapi.Fail(w, r, problem.BadRequest("the "+ControlBackupManifestHeader+" manifest header is required"))
		return
	}
	manifestJSON, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest("the manifest header is not valid base64"))
		return
	}

	gen, err := s.controlBackup.ReceiveBackup(r.Context(), peer.PeerID, peer.PublicKey,
		manifestJSON, io.LimitReader(r.Body, maxControlBackupBody))
	if err != nil {
		// A rejected backup is the pushing peer's problem — a bad signature, a
		// digest mismatch, a source that is not itself. Logged in full here,
		// where the reason is not lost, and returned as a rejection rather than
		// an internal error.
		s.log.Warn("refused a pushed control backup", "peer_id", peer.PeerID, "error", err)
		httpapi.Fail(w, r, problem.BadRequest("the pushed backup was rejected: "+err.Error()))
		return
	}
	s.log.Info("received a control backup from a peer", "peer_id", peer.PeerID, "generation", gen)

	s.writeJSON(w, r, receivedResponse{Generation: gen})
}

// handleControlBackupList answers what this peer holds of the CALLER's control
// plane — the fact against which the sender checks its belief (ADR-0046).
func (s *Server) handleControlBackupList(w http.ResponseWriter, r *http.Request) {
	peer, ok := PeerFrom(r.Context())
	if !ok {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.controlBackup == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"No Control-Plane Store", "this node does not hold control-plane backups for peers"))
		return
	}
	gens, err := s.controlBackup.HeldBackups(peer.PeerID)
	if err != nil {
		s.log.Error("listing held control backups", "peer_id", peer.PeerID, "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	resp := heldResponse{Source: peer.PeerID, Generations: gens}
	if len(gens) > 0 {
		resp.Latest = gens[0]
	}
	s.writeJSON(w, r, resp)
}
