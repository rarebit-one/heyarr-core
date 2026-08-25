package backupsync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
)

// Pusher pushes this node's control-plane backups to the peers that trust it,
// and asks a peer what it holds (§50, ADR-0046). It is the sender half; [Store]
// is the receiver.
type Pusher struct {
	material *mtls.Material
	log      *slog.Logger
}

// NewPusher builds a pusher from this node's certificate material.
func NewPusher(material *mtls.Material, log *slog.Logger) *Pusher {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Pusher{material: material, log: log}
}

// Target is a peer to push to: its pinned identity (for the mTLS handshake) and
// its endpoint (where to dial). The two come from a membership.Member; they are
// kept together here so a caller cannot pin one peer and dial another.
type Target struct {
	Peer     mtls.Peer
	Endpoint string
}

// PushTo pushes the backup in backupDir to one peer and returns the generation
// the peer reports holding afterward.
//
// The manifest rides a header and the snapshot rides the body — the same split
// the receiver expects. Source-side accounting is logged here, in the shape the
// blob and piece serving already use ("... to a peer", bytes, peer_id), so a
// third counter does not disagree with the other two.
func (p *Pusher) PushTo(ctx context.Context, target Target, backupDir string) (int64, error) {
	manifest, err := backup.ReadManifest(backupDir)
	if err != nil {
		return 0, err
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return 0, fmt.Errorf("backupsync: encoding the manifest to push: %w", err)
	}

	origin, err := originFor(target)
	if err != nil {
		return 0, err
	}
	client, closeIdle, err := p.clientFor(target.Peer)
	if err != nil {
		return 0, err
	}
	defer closeIdle()

	snapshot, err := os.Open(filepath.Join(backupDir, backup.SnapshotFile)) //nolint:gosec // a backup path this node produced
	if err != nil {
		return 0, fmt.Errorf("backupsync: opening the snapshot to push: %w", err)
	}
	defer func() { _ = snapshot.Close() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+peerapi.ControlBackupPath, snapshot)
	if err != nil {
		return 0, fmt.Errorf("backupsync: building the push request: %w", err)
	}
	req.Header.Set(peerapi.ControlBackupManifestHeader, base64.StdEncoding.EncodeToString(manifestJSON))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = manifest.SizeBytes

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("backupsync: pushing to %s: %w", target.Peer.PeerID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return 0, fmt.Errorf("backupsync: peer %s refused the backup: %s: %s",
			target.Peer.PeerID, resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		Generation int64 `json:"generation"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&out); err != nil {
		return 0, fmt.Errorf("backupsync: reading the peer's response: %w", err)
	}

	p.log.Info("pushed a control backup to a peer",
		"peer_id", target.Peer.PeerID, "generation", manifest.Generation,
		"bytes", manifest.SizeBytes, "held_generation", out.Generation)
	return out.Generation, nil
}

// HeldBy asks a peer which generations of THIS node's control plane it holds,
// newest first. It is how a source reconciles its belief with the fact.
func (p *Pusher) HeldBy(ctx context.Context, target Target) ([]int64, error) {
	origin, err := originFor(target)
	if err != nil {
		return nil, err
	}
	client, closeIdle, err := p.clientFor(target.Peer)
	if err != nil {
		return nil, err
	}
	defer closeIdle()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+peerapi.ControlBackupPath, nil)
	if err != nil {
		return nil, fmt.Errorf("backupsync: building the query: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backupsync: querying %s: %w", target.Peer.PeerID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backupsync: peer %s answered %s", target.Peer.PeerID, resp.Status)
	}
	var out struct {
		Generations []int64 `json:"generations"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("backupsync: reading held generations: %w", err)
	}
	return out.Generations, nil
}

// clientFor builds a pinned mTLS client for exactly one peer, and a function to
// release its idle connection — this is a one-shot dial, not a pool.
func (p *Pusher) clientFor(peer mtls.Peer) (*http.Client, func(), error) {
	client, err := mtls.Client(mtls.Options{
		Material: p.material,
		Members:  mtls.PinnedKey(peer),
		Logger:   p.log,
	})
	if err != nil {
		return nil, func() {}, fmt.Errorf("backupsync: building a pinned client for %s: %w", peer.PeerID, err)
	}
	closeIdle := func() {
		if tr, ok := client.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	return client, closeIdle, nil
}

// originFor normalises a target's endpoint to an https:// origin.
func originFor(target Target) (string, error) {
	origin, err := endpoint.Normalise(target.Endpoint)
	if err != nil {
		return "", fmt.Errorf("backupsync: peer %s has no usable endpoint: %w", target.Peer.PeerID, err)
	}
	return strings.TrimSuffix(origin, "/"), nil
}
