// Package catalogsync is the outbound half of two-site catalog convergence
// (ADR-0073, #449): the scheduled driver that reaches a sibling's
// /peer/v1/catalog/ops surface over the pinned mTLS link and exchanges the
// editorial op-log both sites hold, so a delete at one site reaches the other
// without an operator triggering it.
//
// It is the mirror of internal/personalstate/replication for the catalog plane:
// the server routes already exist (internal/api/peerapi/catalogops.go); this
// dials them. The op-log is a G-set, so the exchange is a single idempotent,
// order-free POST — this node offers its ops and the sibling answers with the
// merged set, converging both directions in one round trip.
package catalogsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	peerapi "github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// A Target is a sibling to converge with: its pinned identity and where to dial
// it, kept together so a caller cannot pin one peer and dial another (the
// ADR-0012 caution the CAS and state replication keep too).
type Target struct {
	Peer     mtls.Peer
	Endpoint string
}

// maxCatalogOpsBody bounds a sibling's answer into this node's memory. The link
// is authenticated, which is not the same as unbounded: a controller replaced by
// something else is exactly the situation catalog convergence must survive. The
// server's own push limit is 8 MiB; this matches it.
const maxCatalogOpsBody = 8 << 20

// Exchanger offers this node's ops to a sibling and returns the sibling's merged
// log. An interface so the reconcile logic can be tested against a fake without
// standing up mTLS; [Client] is the real, mTLS-pinned implementation.
type Exchanger interface {
	Exchange(ctx context.Context, t Target, ops []string) ([]string, error)
}

// Client is the mTLS-pinned [Exchanger]: it dials a sibling's peer surface with
// this node's certificate, pinned to the sibling's key (ADR-0012).
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

// Exchange POSTs this node's ops to the sibling's /catalog/ops and returns the
// sibling's merged log. One call converges both directions: the sibling records
// what this node offered and answers with its full set (including anything this
// node was missing), which the caller then records locally.
func (c *Client) Exchange(ctx context.Context, t Target, ops []string) ([]string, error) {
	hc, closeIdle, err := c.clientFor(t.Peer)
	if err != nil {
		return nil, err
	}
	defer closeIdle()
	origin, err := originFor(t)
	if err != nil {
		return nil, err
	}
	if ops == nil {
		ops = []string{}
	}
	buf, err := json.Marshal(map[string]any{"ops": ops})
	if err != nil {
		return nil, err
	}
	target := origin + peerapi.Prefix + "/catalog/ops"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalogsync: exchanging ops with %s: %w", t.Peer.PeerID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("catalogsync: %s answered %d exchanging ops: %s",
			t.Peer.PeerID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Ops []string `json:"ops"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCatalogOpsBody)).Decode(&out); err != nil {
		return nil, fmt.Errorf("catalogsync: decoding the merged log from %s: %w", t.Peer.PeerID, err)
	}
	return out.Ops, nil
}

func (c *Client) clientFor(peer mtls.Peer) (*http.Client, func(), error) {
	hc, err := mtls.Client(mtls.Options{Material: c.material, Members: mtls.PinnedKey(peer), Logger: c.log})
	if err != nil {
		return nil, func() {}, fmt.Errorf("catalogsync: building a pinned client for %s: %w", peer.PeerID, err)
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
		return "", fmt.Errorf("catalogsync: peer %s has no usable endpoint: %w", t.Peer.PeerID, err)
	}
	return strings.TrimSuffix(origin, "/"), nil
}
