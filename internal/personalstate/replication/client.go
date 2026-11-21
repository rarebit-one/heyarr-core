package replication

// client.go is the mTLS-pinned Pusher: it dials each Full Peer's state-sync
// surface with this node's certificate and moves opaque metadata, wrapped keys
// and changes. See doc.go for the package's placement and opacity contract.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// PushWrappedKey stores one recipient's wrapped copy, at the key epoch it
	// seals (ADR-0103), on the target. Idempotent. ErrWrapSuperseded when the
	// target holds a newer epoch — the caller skips the copy, it does not fail.
	PushWrappedKey(ctx context.Context, t Target, spaceID, recipient string, wrapped []byte, epoch int) error
	// PushKeyHistory stores one opaque key-history row on the target (ADR-0103).
	// Idempotent on identical bytes.
	PushKeyHistory(ctx context.Context, t Target, spaceID string, epoch int, sealedPrev []byte) error
	// Heads returns the target's causal frontier for a space, so this node can
	// compute what the target is missing and push only that.
	Heads(ctx context.Context, t Target, spaceID string) ([]string, error)
	// PushChange sends one opaque change; the target verifies its content-address.
	PushChange(ctx context.Context, t Target, ch protocol.EncryptedChange) error
	// PushSnapshot sends one opaque snapshot; the target verifies its
	// content-address. Idempotent on the id.
	PushSnapshot(ctx context.Context, t Target, snap protocol.EncryptedSnapshot) error
	// PinBase pins a snapshot the target already holds as its compaction base
	// (#681). Idempotent. ErrBaseSuperseded when the target's base is not
	// covered by it — the caller skips the pin, it does not fail.
	PinBase(ctx context.Context, t Target, spaceID, snapshotID string) error
	// Held asks the target which of ids it holds as change rows of the space, and
	// what base it has pinned (#681, #682). The target confirms each id itself.
	Held(ctx context.Context, t Target, spaceID string, ids []string) (HeldAnswer, error)
}

// A HeldAnswer is what a target confirmed holding of a space: the ids it holds
// as rows, and its pinned compaction base (nil for none), whose causal history
// counts as held too.
type HeldAnswer struct {
	Held []string
	Base *peerapi.StateBase
}

// ErrBaseSuperseded is a base pin the target refused because its own base is not
// covered by the snapshot: this node is behind, or concurrent. It is skipped like
// a superseded wrap, never a failure of the reconcile.
var ErrBaseSuperseded = errors.New("replication: the target's pinned base is not covered by this node's")

// ErrWrapSuperseded is a wrapped copy the target refused because it holds a newer
// key epoch for the space (ADR-0103): this node is behind, and the copy would
// resurrect a key a rotation retired. It is not a failure of the reconcile — the
// copy is skipped and the rest of the space still converges.
var ErrWrapSuperseded = errors.New("replication: the target holds a newer key epoch; the wrapped copy is superseded")

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

// PushWrappedKey POSTs one wrapped copy to /peer/v1/state/{space}/keys. The
// epoch is sent only when non-zero, so an epoch-0 push is byte-for-byte the body
// a peer that predates key epochs (ADR-0103) accepts.
func (c *Client) PushWrappedKey(ctx context.Context, t Target, spaceID, recipient string, wrapped []byte, epoch int) error {
	body := map[string]any{"recipient": recipient, "wrapped": wrapped}
	if epoch != 0 {
		body["epoch"] = epoch
	}
	err := c.post(ctx, t, statePath(spaceID, "/keys"), body)
	var pe *postError
	if errors.As(err, &pe) && pe.status == http.StatusConflict && pe.code == peerapi.CodeKeySuperseded {
		return fmt.Errorf("%w: %w", ErrWrapSuperseded, err)
	}
	return err
}

// PushKeyHistory POSTs one key-history row to /peer/v1/state/{space}/key-history.
func (c *Client) PushKeyHistory(ctx context.Context, t Target, spaceID string, epoch int, sealedPrev []byte) error {
	return c.post(ctx, t, statePath(spaceID, "/key-history"), map[string]any{"epoch": epoch, "sealed_prev": sealedPrev})
}

// PushChange POSTs one opaque change to /peer/v1/state/{space}/changes.
func (c *Client) PushChange(ctx context.Context, t Target, ch protocol.EncryptedChange) error {
	return c.post(ctx, t, statePath(ch.SpaceID, "/changes"), ch)
}

// PushSnapshot POSTs one opaque snapshot to /peer/v1/state/{space}/snapshots.
func (c *Client) PushSnapshot(ctx context.Context, t Target, snap protocol.EncryptedSnapshot) error {
	return c.post(ctx, t, statePath(snap.SpaceID, "/snapshots"), snap)
}

// PinBase POSTs a base pin to /peer/v1/state/{space}/base.
func (c *Client) PinBase(ctx context.Context, t Target, spaceID, snapshotID string) error {
	err := c.post(ctx, t, statePath(spaceID, "/base"), map[string]string{"snapshot_id": snapshotID})
	var pe *postError
	if errors.As(err, &pe) && pe.status == http.StatusConflict && pe.code == peerapi.CodeBaseNotCovering {
		return fmt.Errorf("%w: %w", ErrBaseSuperseded, err)
	}
	return err
}

// Held POSTs the ids to /peer/v1/state/{space}/held in batches of
// peerapi.MaxHeldQuery and gathers the answers.
func (c *Client) Held(ctx context.Context, t Target, spaceID string, ids []string) (HeldAnswer, error) {
	var out HeldAnswer
	// One query even for no ids: it still answers the target's base.
	for start := 0; ; start += peerapi.MaxHeldQuery {
		chunk := ids[start:min(start+peerapi.MaxHeldQuery, len(ids))]
		var resp struct {
			Held []string           `json:"held"`
			Base *peerapi.StateBase `json:"base"`
		}
		if err := c.postJSON(ctx, t, statePath(spaceID, "/held"), map[string][]string{"change_ids": chunk}, &resp); err != nil {
			return HeldAnswer{}, err
		}
		out.Held = append(out.Held, resp.Held...)
		out.Base = resp.Base
		if start+peerapi.MaxHeldQuery >= len(ids) {
			break
		}
	}
	return out, nil
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
	heads, err := decodeHeads(resp.Body, maxHeadsResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("replication: decoding heads from %s: %w", t.Peer.PeerID, err)
	}
	return heads, nil
}

// maxHeadsResponseBytes is a client memory bound, not the peer's per-change
// request limit. The unpaginated frontier can contain every change in a flat
// log (ADR-0101); 64 MiB accommodates over 900,000 canonical BLAKE3 ids. Keep a
// finite ceiling even for an authenticated peer, and report overflow explicitly
// rather than mistaking a truncated response for malformed JSON.
const maxHeadsResponseBytes = 64 << 20

func decodeHeads(body io.Reader, maxBytes int64) ([]string, error) {
	// One extra byte distinguishes an exactly-at-limit response from overflow,
	// including chunked responses and a valid JSON prefix followed by padding.
	raw, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("heads response exceeds %d bytes", maxBytes)
	}
	if err != nil {
		return nil, err
	}
	var out struct {
		Heads []string `json:"heads"`
	}
	// Unmarshal requires one complete JSON document, so a valid prefix cannot
	// hide a second document or trailing garbage. Unknown fields remain allowed.
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Heads, nil
}

// post sends a JSON body and requires a 2xx, closing idle connections after.
func (c *Client) post(ctx context.Context, t Target, path string, body any) error {
	return c.postJSON(ctx, t, path, body, nil)
}

// postJSON is post that also decodes a 2xx answer into out, when out is not nil.
// The answer is bounded like a heads response: a held answer carries the
// target's base frontier, which can be as long as a flat log.
func (c *Client) postJSON(ctx context.Context, t Target, path string, body, out any) error {
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
		var doc struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &doc) // best effort: a non-problem body has no code
		return &postError{
			status: resp.StatusCode,
			code:   doc.Code,
			msg:    fmt.Sprintf("replication: %s answered %d to POST %s: %s", t.Peer.PeerID, resp.StatusCode, path, strings.TrimSpace(string(raw))),
		}
	}
	if out == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxHeadsResponseBytes+1))
	if err != nil {
		return fmt.Errorf("replication: reading %s's answer to POST %s: %w", t.Peer.PeerID, path, err)
	}
	if int64(len(raw)) > maxHeadsResponseBytes {
		return fmt.Errorf("replication: %s's answer to POST %s exceeds %d bytes", t.Peer.PeerID, path, maxHeadsResponseBytes)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("replication: decoding %s's answer to POST %s: %w", t.Peer.PeerID, path, err)
	}
	return nil
}

// postError is a non-2xx answer to a push, carrying the status and the problem
// document's code so a caller can branch on a refusal it expects.
type postError struct {
	status int
	code   string
	msg    string
}

func (e *postError) Error() string { return e.msg }
