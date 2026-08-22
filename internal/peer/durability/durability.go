// Package durability answers, over the network, the question garbage
// collection may not answer from a table: does another peer actually hold
// these bytes (ADR-0018, §19, §53, §56, M4-12).
//
// # Why this is a network call and not a query
//
// The catalog already has an opinion. `replicas` says which peers hold which
// blobs, it is indexed, it is fast, and reading it would have been the obvious
// implementation of "confirm the placement policy is satisfied elsewhere".
//
// It would also have been wrong, and wrong in the one direction that cannot be
// undone. A `replicas` row is the CONTROLLER'S BELIEF about a machine it is not
// — the whole premise of Milestone 4 is that a second Full Peer creates the
// opportunity for beliefs and bytes to diverge, and migration 00023 exists
// because they do. A peer that lost a disk, restored an older CAS, quarantined
// a blob or was simply never asked leaves a row that still says `present`.
// Deleting the last local copy on the strength of that row is deleting content
// because a table said someone else probably has it.
//
// So before the last local copy goes, the remote copy is verified to exist. It
// costs one HEAD request against a blob that was already unreferenced for a
// week, which is nothing, and it converts the one irreversible operation in
// Heyarr from a belief into an observation.
//
// # What each answer means, and why three of them
//
//   - nothing answered            -> ErrPeerUnreachable. Establishes nothing.
//   - answered 404                -> ErrPeerLacksBlob. The row was a lie, and
//     the collector corrects it to `missing` on the way past.
//   - answered 200                -> the only outcome that permits a delete.
//
// Collapsing the first two is the tempting simplification and it removes a
// refusal: "peer is down" and "peer does not have it" call for different
// actions and produce different operator work, and a caller that cannot tell
// them apart cannot correct a lying row.
//
// # The controller half
//
// §53's degraded-operation table says "delete replicas: No" during a controller
// outage. Garbage collection ran on local SQLite and a local CAS and consulted
// neither the controller nor anything that knew what degraded meant, so a peer
// cut off from the control plane would happily unlink — which is precisely the
// scenario ADR-0018 warns about, reachable by anybody with a shell. [Controller]
// is the seam that makes that sentence enforceable.
package durability

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// maxRefusalBody bounds how much of a peer's refusal is read back into an
// error, exactly as internal/peer/transfer bounds it: a problem document is a
// few hundred bytes, and a peer answering a HEAD with a gigabyte is not
// something to put in a log line.
const maxRefusalBody = 8 << 10

// Controller is how this node establishes that the control plane is there.
//
// An interface rather than a URL, because "the controller" is co-located with
// the collector in every deployment shape that exists today (`heyarr all`,
// `heyarr gc`, the worker) and is a remote HTTP surface in the ones that do
// not yet. Ping returns nil only when the control plane actually answered.
type Controller interface {
	Ping(ctx context.Context) error
}

// ControllerFunc adapts a function to [Controller].
type ControllerFunc func(ctx context.Context) error

// Ping implements Controller.
func (f ControllerFunc) Ping(ctx context.Context) error { return f(ctx) }

// LocalControlPlane is the Controller for a collector running alongside its
// own single-writer control plane (ADR-0003).
//
// It exercises the WRITER rather than the reader. Reading is served from a
// snapshot and a peer holding a catalog snapshot (#145) can read all day
// without the control plane being there at all — which is exactly the degraded
// node §53 is about. Being able to open a write transaction is the closest
// available statement of "the control plane is present and mine".
//
// It does not write anything. `BEGIN IMMEDIATE` through a read of the writer
// connection is enough to prove the file is there, is not locked away on a
// vanished mount, and answers.
func LocalControlPlane(writer *sql.DB) Controller {
	return ControllerFunc(func(ctx context.Context) error {
		if writer == nil {
			return fmt.Errorf("%w: this collector has no control plane to reach",
				integrity.ErrControllerUnreachable)
		}
		var one int
		if err := writer.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
			return fmt.Errorf("%w: %w", integrity.ErrControllerUnreachable, err)
		}
		return nil
	})
}

// Options configure a Verifier.
type Options struct {
	// Material is this node's certificate, and therefore its identity to the
	// peer it asks. Required: the peer fabric authenticates by certificate
	// only (ADR-0012).
	Material *mtls.Material
	// Controller is how the control plane is reached. Required — a Verifier
	// that could not answer §53's question would answer it by omission, which
	// is the failing-open shape this whole issue is about.
	Controller Controller
	Logger     *slog.Logger
}

// Verifier implements [integrity.Durability] against real peers.
type Verifier struct {
	material   *mtls.Material
	controller Controller
	log        *slog.Logger
}

var _ integrity.Durability = (*Verifier)(nil)

// New builds a Verifier, or explains what is missing.
func New(opts Options) (*Verifier, error) {
	switch {
	case opts.Material == nil:
		return nil, errors.New("durability: this node's certificate material is required — the " +
			"peer fabric authenticates by certificate only (ADR-0012)")
	case opts.Controller == nil:
		return nil, errors.New("durability: a controller is required — §53 says a peer cut off " +
			"from the control plane does not delete replicas, and a verifier that cannot check " +
			"that would let the check pass by being absent")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Verifier{
		material:   opts.Material,
		controller: opts.Controller,
		log:        log.With("component", "durability"),
	}, nil
}

// Controller implements [integrity.Durability].
func (v *Verifier) Controller(ctx context.Context) error { return v.controller.Ping(ctx) }

// Holds asks one peer whether it serves these bytes right now.
//
// A HEAD rather than a GET: the question is existence, the blob may be sixty
// gigabytes, and the peer surface answers HEAD on the same route with the same
// validators (internal/api/blobs). Nothing about the response body is
// consulted, and nothing about the response is trusted as identity — the hash
// in the path came from this node's own catalog, never from anything a peer
// said.
func (v *Verifier) Holds(ctx context.Context, p integrity.Peer, h hashing.Hash) error {
	if len(p.PublicKey) == 0 {
		// Refused before a socket exists, exactly as a replication pull is.
		// Membership is the only trust root in the inter-peer path, and a peer
		// with no pinned key is one membership cannot vouch for — dialling it
		// and believing whatever answered would be trust on first use, in the
		// service of deciding it is safe to delete (ADR-0012).
		return fmt.Errorf("durability: peer %s has no pinned public key, so nothing it said could "+
			"be attributed to it (ADR-0012)", p.Name)
	}
	origin, err := endpoint.Normalise(p.Endpoint)
	if err != nil {
		return fmt.Errorf("durability: peer %s is recorded at an endpoint this node cannot dial: %w",
			p.Name, err)
	}
	if !strings.HasPrefix(origin, endpoint.Scheme+"://") {
		return fmt.Errorf("%w: peer %s is at %q, which is not an %s origin, and the peer surface is "+
			"mutually authenticated TLS (ADR-0012)", endpoint.ErrMalformed, p.Name, p.Endpoint,
			endpoint.Scheme)
	}

	client, err := mtls.Client(mtls.Options{
		Material: v.material,
		Members: mtls.PinnedKey(mtls.Peer{
			PeerID: p.PeerID, Name: p.Name, PublicKey: p.PublicKey,
		}),
		Logger: v.log,
	})
	if err != nil {
		return fmt.Errorf("durability: building a pinned client for peer %s: %w", p.Name, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		origin+peerapi.BlobContentPath(h.String()), nil)
	if err != nil {
		return fmt.Errorf("durability: building the check of %s against peer %s: %w", h, p.Name, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Silence, in the sense internal/peer/health means it: nothing
		// answered. Not a status, not an error document — no response at all.
		return fmt.Errorf("%w: peer %s at %s: %w", integrity.ErrPeerUnreachable, p.Name, origin, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// A HEAD has no body worth reading, but a peer is free to send one and an
	// unread body is a connection that cannot be reused.
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusalBody))

	switch resp.StatusCode {
	case http.StatusOK:
		v.log.Debug("a peer confirmed it holds a blob",
			"blob_hash", h.String(), "peer_id", p.PeerID, "peer_name", p.Name)
		return nil
	case http.StatusNotFound:
		// The row said present and the peer says otherwise. This is the case
		// the whole package exists for.
		return fmt.Errorf("%w: peer %s answered 404 for %s", integrity.ErrPeerLacksBlob, p.Name, h)
	case http.StatusServiceUnavailable:
		// The peer is up and serves no bytes at all. It has not said it lacks
		// the blob, so its row is not corrected; it has not stayed silent, so
		// it is not unreachable. Nothing was established either way, which is
		// a refusal.
		return fmt.Errorf("durability: peer %s is not serving blob content on the peer fabric, so "+
			"it cannot confirm anything about %s", p.Name, h)
	default:
		return fmt.Errorf("durability: peer %s answered %d when asked about %s: %s",
			p.Name, resp.StatusCode, h, strings.TrimSpace(string(detail)))
	}
}
