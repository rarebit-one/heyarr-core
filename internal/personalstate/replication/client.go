package replication

// client.go is the mTLS-pinned Pusher: it dials each Full Peer's state-sync
// surface with this node's certificate and moves opaque metadata, wrapped keys
// and changes. See doc.go for the package's placement and opacity contract.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	peerapi "github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

// A Target is a Full Peer to replicate to: its pinned identity and where to dial
// it, kept together so a caller cannot pin one peer and dial another (the
// ADR-0012 caution the CAS replication keeps too).
type Target struct {
	Peer     mtls.Peer
	Endpoint string
}

// Pusher moves opaque personal state to one Full Peer over its state-sync
// surface. It is an interface so the reconcile logic can be tested against a
// fake target without standing up mTLS; [Client] is the real, mTLS-pinned
// implementation.
type Pusher interface {
	// PushSpace records a space's identity (id + kind) on the target. Idempotent.
	PushSpace(ctx context.Context, t Target, spaceID, kind string) error
	// PushWrappedKey stores one recipient's wrapped copy on the target. Idempotent.
	PushWrappedKey(ctx context.Context, t Target, spaceID, recipient string, wrapped []byte) error
	// Heads returns the target's causal frontier for a space, so this node can
	// compute what the target is missing and push only that.
	Heads(ctx context.Context, t Target, spaceID string) ([]string, error)
	// PushChange sends one opaque change; the target verifies its content-address.
	PushChange(ctx context.Context, t Target, ch protocol.EncryptedChange) error
}

// Client is the mTLS-pinned [Pusher]: it dials each target's peer surface with
// this node's certificate, pinned to the target's key (ADR-0012).
type Client struct {
	material *mtls.Material
	log      *slog.Logger
}

// NewClient builds a client that dials with the given certificate material.
func NewClient(material *mtls.Material, log *slog.Logger) *Client {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{material: material, log: log}
}

func (c *Client) clientFor(peer mtls.Peer) (*http.Client, func(), error) {
	hc, err := mtls.Client(mtls.Options{Material: c.material, Members: mtls.PinnedKey(peer), Logger: c.log})
	if err != nil {
		return nil, func() {}, fmt.Errorf("replication: building a pinned client for %s: %w", peer.PeerID, err)
	}
	closeIdle := func() {
		if tr, ok := hc.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	return hc, closeIdle, nil
}

func originFor(t Target) (string, error) {
	origin, err := endpoint.Normalise(t.Endpoint)
	if err != nil {
		return "", fmt.Errorf("replication: peer %s has no usable endpoint: %w", t.Peer.PeerID, err)
	}
	return strings.TrimSuffix(origin, "/"), nil
}

func statePath(spaceID, suffix string) string {
	return peerapi.Prefix + "/state/" + url.PathEscape(spaceID) + suffix
}

// PushSpace POSTs the space's kind to /peer/v1/state/{space}.
func (c *Client) PushSpace(ctx context.Context, t Target, spaceID, kind string) error {
	return c.post(ctx, t, statePath(spaceID, ""), map[string]string{"kind": kind})
}

// PushWrappedKey POSTs one wrapped copy to /peer/v1/state/{space}/keys.
func (c *Client) PushWrappedKey(ctx context.Context, t Target, spaceID, recipient string, wrapped []byte) error {
	return c.post(ctx, t, statePath(spaceID, "/keys"), map[string]any{"recipient": recipient, "wrapped": wrapped})
}

// PushChange POSTs one opaque change to /peer/v1/state/{space}/changes.
func (c *Client) PushChange(ctx context.Context, t Target, ch protocol.EncryptedChange) error {
	return c.post(ctx, t, statePath(ch.SpaceID, "/changes"), ch)
}

// Heads GETs the target's causal frontier from /peer/v1/state/{space}/heads.
func (c *Client) Heads(ctx context.Context, t Target, spaceID string) ([]string, error) {
	hc, closeIdle, err := c.clientFor(t.Peer)
	if err != nil {
		return nil, err
	}
	defer closeIdle()
	origin, err := originFor(t)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+statePath(spaceID, "/heads"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("replication: offering heads to %s: %w", t.Peer.PeerID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("replication: %s answered %d offering heads for %s", t.Peer.PeerID, resp.StatusCode, spaceID)
	}
	var out struct {
		Heads []string `json:"heads"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("replication: decoding heads from %s: %w", t.Peer.PeerID, err)
	}
	return out.Heads, nil
}

// post sends a JSON body and requires a 2xx, closing idle connections after.
func (c *Client) post(ctx context.Context, t Target, path string, body any) error {
	hc, closeIdle, err := c.clientFor(t.Peer)
	if err != nil {
		return err
	}
	defer closeIdle()
	origin, err := originFor(t)
	if err != nil {
		return err
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("replication: POST %s to %s: %w", path, t.Peer.PeerID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("replication: %s answered %d to POST %s: %s", t.Peer.PeerID, resp.StatusCode, path, strings.TrimSpace(string(raw)))
	}
	return nil
}
