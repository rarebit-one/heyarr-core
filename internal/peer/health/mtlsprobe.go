package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// ErrNoPinnedKey is a peer that has an endpoint and no key to pin it with.
//
// It is a refusal to dial rather than a probe failure, and the distinction
// matters: dialling an address and believing whatever answered is trust on
// first use, which is exactly what ADR-0012 removed from the inter-peer path.
// A peer enrolled without a key is a configuration gap, and it must not be
// reported as a peer that is up.
var ErrNoPinnedKey = errors.New("health: the peer has no pinned public key, so nothing it answered could be attributed to it (ADR-0012)")

// MTLSProber probes a peer over the peer fabric itself — pinned mutual TLS,
// with this node's certificate, against the key membership records (#184).
//
// # Why the plain-HTTPS prober could not do this
//
// [HTTPProber] speaks ordinary HTTP(S) and asks for /healthz. A peer surface
// is an mTLS listener with ClientAuth: RequireAnyClientCert, so it never
// completes a handshake with a client that presents no certificate — and it
// serves no /healthz either. Against the topology M4 actually builds, the
// probe therefore could not answer, ever. Combined with a peer surface that
// recorded no liveness of its own, a remote peer's stored health never left
// `unknown`, and everything downstream of it — read routing's health filter
// (#146), garbage collection's durability check (#144) — was running on an
// input that could not move (#184).
//
// The probe's CONTRACT is unchanged, and it is [Prober]'s: it returns nil if
// anything answered, whatever it answered. The peer surface's 404 for an
// unknown path and its 403 for a revoked member are both peers that are up.
// Only silence — a refused connection, no route, a failed handshake, a timeout
// — is an error, and even then nothing is recorded anywhere: see Sweep.
//
// # What it asks for
//
// The peer surface's identity route. It is the cheapest thing on that surface,
// it touches no CAS and no content, and it is already the route M4-05 built to
// answer "who do you think I am" — so a probe is a request the fabric already
// makes rather than one this package invented. The answer is not read: the
// question is whether a process is listening, not whether it agrees.
//
// # Endpoints that are not the peer fabric
//
// A peer recorded at unix:// or http:// is not an mTLS peer surface — it is
// the single-host endpoint a node derives for itself (config.PeerEndpoint), or
// a development address. Those are delegated to the embedded [HTTPProber]
// unchanged, so the topology that worked before this type existed still works.
// Only an https:// endpoint, which is what `heyarr peers add` normalises a
// real peer to, gets the pinned dial.
type MTLSProber struct {
	// Material is this node's certificate, and therefore its identity to the
	// peer it probes. Required: the peer fabric authenticates by certificate
	// only (ADR-0012).
	Material *mtls.Material
	// Timeout bounds one probe. Zero means DefaultProbeTimeout.
	Timeout time.Duration
	// Plain probes the endpoints that are not peer surfaces — see the type
	// doc. Its zero value is usable.
	Plain HTTPProber
	// Logger records what a probe learned, at debug.
	Logger *slog.Logger
}

var _ Prober = MTLSProber{}

// Probe returns nil if the peer answered, whatever it answered.
func (p MTLSProber) Probe(ctx context.Context, peer Peer) error {
	raw := strings.TrimSpace(peer.Endpoint)
	if raw == "" {
		return ErrNoEndpoint
	}
	// Not a peer surface. See the type doc: this is the local socket a
	// single-host deployment derives for itself, or a development address, and
	// it is probed exactly as it was before this type existed. It is decided
	// on the raw value because endpoint.Normalise refuses a non-https scheme
	// outright — correctly, for enrolment, which is a different question from
	// what a probe may speak.
	if strings.HasPrefix(raw, "unix://") || strings.HasPrefix(raw, "http://") {
		plain := p.Plain
		if plain.Timeout <= 0 {
			// One probe is one deadline, whichever door it goes through. A
			// delegate that quietly used a different bound would make a sweep's
			// worst case depend on which endpoints happen to be enrolled.
			plain.Timeout = p.Timeout
		}
		return plain.Probe(ctx, peer)
	}
	// Everything else must normalise to an https origin: a bare host:port is
	// completed to one, and anything that cannot be is a configuration gap
	// reported as such rather than dialled hopefully.
	origin, err := endpoint.Normalise(raw)
	if err != nil {
		return fmt.Errorf("health: peer %s is recorded at an endpoint this node cannot dial: %w",
			peer.Name, err)
	}
	if len(peer.PublicKey) == 0 {
		return fmt.Errorf("%w: peer %s at %s", ErrNoPinnedKey, peer.Name, origin)
	}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	log := p.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	// The pinned client, built through mtls.Client because that is the only
	// supported way to make a peer request: a hand-rolled http.Client here
	// would be one line away from an unpinned transport, and the traffic would
	// look identical until the day it mattered.
	client, err := mtls.Client(mtls.Options{
		Material: p.Material,
		Members: mtls.PinnedKey(mtls.Peer{
			PeerID: peer.PeerID, Name: peer.Name, PublicKey: peer.PublicKey,
		}),
		Logger: log,
	})
	if err != nil {
		return fmt.Errorf("health: building a pinned client to probe peer %s: %w", peer.Name, err)
	}
	// A transport this call created holds an idle connection afterwards, and
	// there is no next probe on it to reuse one — the next probe is minutes
	// away and builds its own. Releasing it keeps a peer that is probed all
	// week from accumulating a socket per probe.
	defer func() {
		if tr, ok := client.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+peerapi.IdentityPath, nil)
	if err != nil {
		return fmt.Errorf("health: building a probe for %s: %w", peer.Name, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Silence, in the sense this package means it: nothing answered. This
		// is the ONLY thing that makes a peer unreachable, and it does so by
		// not calling Answered rather than by recording a failure anywhere.
		return fmt.Errorf("health: probing peer %s at %s: %w", peer.Name, origin, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drained so the connection is reusable, and capped so a peer answering
	// with a gigabyte cannot turn a probe into a transfer.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	// resp.StatusCode is not examined, here or anywhere. A 403 from a peer
	// that has revoked us is a peer that is up, and reporting it as an outage
	// would be an authentication problem wearing an outage's clothes.
	log.Debug("a peer answered a probe over the peer fabric",
		"peer_id", peer.PeerID, "peer_name", peer.Name, "endpoint", origin, "status", resp.StatusCode)
	return nil
}
