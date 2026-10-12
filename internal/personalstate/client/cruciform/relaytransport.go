package cruciform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rarebit-one/void-which-binds-go/relay"
)

// The offload exchange rides the void-which-binds pairing relay (ADR-0002) as a second
// two-party protocol: the desktop is the "initiator", the phone the "responder",
// and the two message slots are the request and the response. A relay carrying
// the offload must accept these slot names (relay.Options.Types) alongside its
// pairing types.
const (
	RelayRequestType  = "unwrap-req"
	RelayResponseType = "unwrap-resp"
)

// RelayUnwrapTypes are the relay message-slot names the offload exchange adds. A
// relay that carries both pairing and offload declares
// append(relay.DefaultTypes, RelayUnwrapTypes...).
var RelayUnwrapTypes = []string{RelayRequestType, RelayResponseType}

// RelayPairTypes are the relay message-slot names the offload PAIRING ceremony
// adds beyond the default pairing set (relay.DefaultTypes already carries
// "commit" and "reveal"; the offload pairing also posts a mutual "confirm"). A
// node relay that carries the offload live path mounts
// append(relay.DefaultTypes, append(RelayPairTypes, RelayUnwrapTypes...)...).
var RelayPairTypes = []string{pairMsgConfirm}

// WakeFunc wakes the paired phone so it opens the given relay session for an
// unwrap. In production it asks the node (POST /v1/unwrap-wake), which asks the
// shared notify plane to fan an opaque void-which-binds:unwrap?relay=&session= ping to
// the user's devices (ADR-0102); a nil WakeFunc skips the wake, for when the phone is
// already reachable (a LAN-direct path, or a test with the phone already polling).
type WakeFunc func(ctx context.Context, relayBase, session string) error

// RelayTransport is the offload [Transport] over the void-which-binds relay plus a wake.
// On each unwrap it creates a fresh relay session, posts the desktop's signed
// request into it, wakes the phone at that session, and polls for the phone's
// signed, sealed reply. It carries only opaque bytes — the request is signed and
// carries no secret, the reply is sealed to a per-unwrap key — so the relay stays
// blind (ADR-0098).
type RelayTransport struct {
	// RelayBase is the relay origin the desktop and phone share.
	RelayBase string
	// Wake wakes the phone at the session created for this unwrap; nil skips it.
	// The wake is best-effort (#688): a failed wake is reported through
	// WakeFailed and the round-trip keeps waiting, because the phone may
	// already be awake on the relay or poll it shortly.
	Wake WakeFunc
	// WakeFailed, if set, is told about a failed wake as it happens, so the
	// operator sees it (a refused credential above all) while the round-trip
	// still waits for the phone.
	WakeFailed func(error)
	// HTTP is the client for relay requests; nil uses http.DefaultClient.
	HTTP *http.Client
	// Poll overrides the relay client's default poll interval when non-zero.
	Poll time.Duration
}

var _ Transport = (*RelayTransport)(nil)

// RoundTrip implements [Transport]: one relay session carries one request out and
// one response back.
func (t *RelayTransport) RoundTrip(ctx context.Context, request []byte) ([]byte, error) {
	session, err := relay.CreateSession(ctx, t.HTTP, t.RelayBase)
	if err != nil {
		return nil, fmt.Errorf("cruciform: creating the relay session: %w", err)
	}
	desktop := &relay.Client{
		Base:         t.RelayBase,
		Session:      session,
		Role:         "initiator",
		HTTP:         t.HTTP,
		PollInterval: t.Poll,
	}
	if err := desktop.Post(ctx, RelayRequestType, request); err != nil {
		return nil, fmt.Errorf("cruciform: posting the unwrap request: %w", err)
	}
	// The wake runs beside the poll, not before it: a node or plane that stalls
	// rather than failing must not spend the deadline before Fetch starts, and
	// the phone may answer while the wake is still in flight (Codex on #739).
	// It is cancelled once the round-trip ends and always awaited, so it never
	// outlives RoundTrip; a cancellation of ours is not a failed wake.
	wakeCtx, cancelWake := context.WithCancel(ctx)
	wakeDone := make(chan error, 1)
	if t.Wake != nil {
		go func() {
			err := t.Wake(wakeCtx, t.RelayBase, session)
			if wakeCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				err = nil // our cancellation, not a failed wake; anything else is still reported (Codex on #739)
			}
			if err != nil && t.WakeFailed != nil {
				t.WakeFailed(err)
			}
			wakeDone <- err
		}()
	} else {
		wakeDone <- nil
	}
	// Fetch polls the PEER (responder) slot until the phone posts its reply or ctx
	// is done — the phone is human-paced (a biometric approval), so "not yet" is
	// the normal case, not an error.
	resp, err := desktop.Fetch(ctx, RelayResponseType)
	cancelWake()
	wakeErr := <-wakeDone
	if err != nil {
		if wakeErr != nil {
			return nil, fmt.Errorf("cruciform: awaiting the phone's reply after a failed wake (%w): %w", wakeErr, err)
		}
		return nil, fmt.Errorf("cruciform: awaiting the phone's reply: %w", err)
	}
	return resp, nil
}
