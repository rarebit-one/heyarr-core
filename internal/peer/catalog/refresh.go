package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Fetcher obtains the next snapshot payload from the controller.
//
// An interface because the transport and the materialisation are different
// concerns with different failure modes, and because a test that has to stand
// up TLS to assert a prune is a test nobody will write a second one of. The
// real implementation is [HTTPFetcher], over the authenticated peer link
// (M4-06).
type Fetcher interface {
	// Fetch asks for the snapshot after holding. holding is zero when this
	// peer has none, which is also what a peer whose snapshot store was lost
	// reports — and both correctly produce a full rebuild.
	Fetch(ctx context.Context, holding int64, full bool) (*Snapshot, error)
}

// Refresher keeps a peer's snapshot current.
//
// It owns the only writable handle in the system, which is what makes "no
// writer but the snapshot builder" a fact about the process rather than a
// convention (§52).
type Refresher struct {
	store *Store
	fetch Fetcher
}

// NewRefresher wires a fetcher to the one writable store.
func NewRefresher(store *Store, fetch Fetcher) (*Refresher, error) {
	if store == nil {
		return nil, errors.New("catalog: a refresher needs a snapshot store")
	}
	if !store.Writable() {
		return nil, fmt.Errorf("%w: a refresher needs the writable handle from Open, "+
			"not a read handle", ErrReadOnly)
	}
	if fetch == nil {
		return nil, errors.New("catalog: a refresher needs a source to fetch from")
	}
	return &Refresher{store: store, fetch: fetch}, nil
}

// Refresh brings the snapshot up to date and reports what it now holds.
//
// Passing full forces the drift-correcting full rebuild — the same escape
// hatch M4-07 gives inventory reporting, and for the same reason: when the
// materialised view and its source disagree and nobody can say why, the way
// out is to stop being incremental.
//
// A peer with no snapshot asks from zero, which the controller answers with a
// full payload. It does NOT ask from "the empty snapshot", because there is no
// such thing here: absent and empty are different states and only one of them
// is a snapshot (see [ErrNoSnapshot]).
func (r *Refresher) Refresh(ctx context.Context, full bool) (Meta, error) {
	var holding int64
	switch current, err := r.store.Metadata(ctx); {
	case errors.Is(err, ErrNoSnapshot):
		holding = 0
	case err != nil:
		return Meta{}, err
	default:
		holding = current.Version
	}

	snap, err := r.fetch.Fetch(ctx, holding, full)
	if err != nil {
		return Meta{}, err
	}
	if snap == nil {
		return Meta{}, errors.New("catalog: the controller answered with no snapshot")
	}
	if err := r.store.Apply(ctx, snap); err != nil {
		return Meta{}, err
	}
	return r.store.Metadata(ctx)
}

// HTTPFetcher pulls a snapshot over the authenticated peer link (M4-06).
//
// The client must be one built by internal/peer/mtls — the transport is pinned
// to membership, and a plain http.Client here would be an unpinned connection
// carrying a complete description of the library's organisation.
type HTTPFetcher struct {
	// Client is a pinned peer client (mtls.Client).
	Client *http.Client
	// BaseURL is the controller's peer surface, e.g. https://host:port/peer/v1.
	BaseURL string
}

// maxSnapshotBody bounds what a controller may stream into this peer's memory.
//
// The link is authenticated, and authenticated is not the same as unbounded: a
// controller that has been replaced by something else is exactly the situation
// a peer is holding a snapshot for. 256 MiB is far beyond a catalogue's JSON
// and far below a memory problem.
const maxSnapshotBody = 256 << 20

// Fetch requests the snapshot after holding.
func (f HTTPFetcher) Fetch(ctx context.Context, holding int64, full bool) (*Snapshot, error) {
	if f.Client == nil {
		return nil, errors.New("catalog: fetching a snapshot needs a pinned peer client (mtls.Client)")
	}
	target := strings.TrimSuffix(f.BaseURL, "/") + "/catalog/snapshot"
	q := url.Values{}
	q.Set("holding", strconv.FormatInt(holding, 10))
	if full {
		q.Set("full", "true")
	}
	target += "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("catalog: building the snapshot request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog: fetching the snapshot: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("catalog: the controller refused the snapshot request: %s: %s",
			resp.Status, strings.TrimSpace(string(body)))
	}

	var snap Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSnapshotBody)).Decode(&snap); err != nil {
		return nil, fmt.Errorf("catalog: decoding the snapshot: %w", err)
	}
	if err := snap.Meta.Validate(); err != nil {
		return nil, err
	}
	return &snap, nil
}
