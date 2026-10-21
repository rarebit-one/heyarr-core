// Package leasesync is the fetching half of the access-lease cache (ADR-0048,
// #285): on a cadence it reaches each sibling's GET /peer/v1/leases over the
// pinned mTLS link and mirrors the device write leases that sibling serves into
// this peer's cache, so this peer can still lift a device to write here once
// that sibling is unreachable.
//
// The serving route already exists (internal/api/peerapi/leases.go) and the
// cache already exists (leases.CacheStore); this dials the one and fills the
// other. Nothing here authorises a request: the cache is consulted on the
// request path by later work.
package leasesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	peerapi "github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// A Target is a sibling to fetch from: its pinned identity and where to dial
// it, kept together so a caller cannot pin one peer and dial another (the
// ADR-0012 caution catalog sync keeps too).
type Target struct {
	Peer     mtls.Peer
	Endpoint string
}

// maxLeasesBody bounds a sibling's answer into this node's memory. A device
// write lease token is a few hundred bytes and a sibling serves one per
// write-authorised device per site, so 4 MiB is thousands of devices of
// headroom while still refusing a sibling that answers without end.
const maxLeasesBody = 4 << 20

// Fetcher returns the lease tokens a sibling serves. An interface so the sync
// logic can be tested against a fake; [Client] is the mTLS-pinned one.
type Fetcher interface {
	FetchLeases(ctx context.Context, t Target) ([]string, error)
}

// Client is the mTLS-pinned [Fetcher]: it dials a sibling's peer surface with
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

// FetchLeases GETs the sibling's /leases and returns the tokens it served. Any
// answer but a 2xx with a decodable body is an error, including the 503 a
// sibling that issues no leases answers: the caller keeps what it had cached.
func (c *Client) FetchLeases(ctx context.Context, t Target) ([]string, error) {
	hc, err := mtls.Client(mtls.Options{Material: c.material, Members: mtls.PinnedKey(t.Peer), Logger: c.log})
	if err != nil {
		return nil, fmt.Errorf("leasesync: building a pinned client for %s: %w", t.Peer.PeerID, err)
	}
	defer func() {
		if tr, ok := hc.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}()
	origin, err := endpoint.Normalise(t.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("leasesync: peer %s has no usable endpoint: %w", t.Peer.PeerID, err)
	}
	target := strings.TrimSuffix(origin, "/") + peerapi.Prefix + "/leases"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("leasesync: fetching leases from %s: %w", t.Peer.PeerID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("leasesync: %s answered %d for its leases: %s",
			t.Peer.PeerID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return decodeLeases(resp.Body, t.Peer.PeerID)
}

// decodeLeases reads a sibling's lease set and refuses anything that is not
// exactly one complete JSON object carrying a `leases` field. A fetch that
// decodes is AUTHORITATIVE (Refresh replaces the sibling's cached set with
// it), so a truncated, oversized or trailing-garbage body must be a failed
// fetch that keeps the cache, never a short set that wipes it.
func decodeLeases(body io.Reader, peerID string) ([]string, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxLeasesBody+1))
	if err != nil {
		return nil, fmt.Errorf("leasesync: reading the leases from %s: %w", peerID, err)
	}
	if len(raw) > maxLeasesBody {
		return nil, fmt.Errorf("leasesync: %s answered more than %d bytes of leases", peerID, maxLeasesBody)
	}
	var out struct {
		Leases *[]string `json:"leases"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("leasesync: decoding the leases from %s: %w", peerID, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("leasesync: %s answered data after its lease set", peerID)
	}
	if out.Leases == nil {
		// A body without the field is not "serves none": reading it that way
		// would wipe the cache on a malformed answer.
		return nil, fmt.Errorf("leasesync: %s answered without a leases field", peerID)
	}
	return *out.Leases, nil
}
