package placementsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// maxOfferBody bounds an answer. One range is a sixteenth of what a source
// offers; at roughly 100 bytes an entry, 16 MiB is about 160,000 blobs in one
// range, far past a household vault, and still a bound.
const maxOfferBody = 16 << 20

// Client is the mTLS-pinned [Asker]: it dials a sibling's peer surface with
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

// Ask POSTs one question to the sibling and decodes its answer.
func (c *Client) Ask(ctx context.Context, t Target, a Ask) (Offer, error) {
	hc, err := mtls.Client(mtls.Options{Material: c.material, Members: mtls.PinnedKey(t.Peer), Logger: c.log})
	if err != nil {
		return Offer{}, fmt.Errorf("placementsync: building a pinned client for %s: %w", t.Peer.PeerID, err)
	}
	defer func() {
		if tr, ok := hc.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}()
	origin, err := endpoint.Normalise(t.Endpoint)
	if err != nil {
		return Offer{}, fmt.Errorf("placementsync: peer %s has no usable endpoint: %w", t.Peer.PeerID, err)
	}
	body, err := json.Marshal(a)
	if err != nil {
		return Offer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(origin, "/")+Path, bytes.NewReader(body))
	if err != nil {
		return Offer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return Offer{}, fmt.Errorf("placementsync: asking %s: %w", t.Peer.PeerID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return Offer{}, fmt.Errorf("placementsync: %s answered %d: %s",
			t.Peer.PeerID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var offer Offer
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOfferBody)).Decode(&offer); err != nil {
		return Offer{}, fmt.Errorf("placementsync: decoding the answer from %s: %w", t.Peer.PeerID, err)
	}
	return offer, nil
}
