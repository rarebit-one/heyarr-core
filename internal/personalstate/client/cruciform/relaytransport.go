package cruciform

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rarebit-one/voidbind-go/relay"
)

// The offload exchange rides the voidbind pairing relay (ADR-0002) as a second
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

// WakeFunc wakes the paired phone so it opens the given relay session for an
// unwrap. In production it asks the controller to fan an opaque
// voidbind:unwrap?relay=&session= ping to the user's devices
// (notify.EnqueueUnwrap); a nil WakeFunc skips the wake, for when the phone is
// already reachable (a LAN-direct path, or a test with the phone already polling).
type WakeFunc func(ctx context.Context, relayBase, session string) error

// RelayTransport is the offload [Transport] over the voidbind relay plus a wake.
// On each unwrap it creates a fresh relay session, posts the desktop's signed
// request into it, wakes the phone at that session, and polls for the phone's
// signed, sealed reply. It carries only opaque bytes — the request is signed and
// carries no secret, the reply is sealed to a per-unwrap key — so the relay stays
// blind (ADR-0098).
type RelayTransport struct {
	// RelayBase is the relay origin the desktop and phone share.
	RelayBase string
	// Wake wakes the phone at the session created for this unwrap; nil skips it.
	Wake WakeFunc
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
	if t.Wake != nil {
		if err := t.Wake(ctx, t.RelayBase, session); err != nil {
			return nil, fmt.Errorf("cruciform: waking the phone: %w", err)
		}
	}
	// Fetch polls the PEER (responder) slot until the phone posts its reply or ctx
	// is done — the phone is human-paced (a biometric approval), so "not yet" is
	// the normal case, not an error.
	resp, err := desktop.Fetch(ctx, RelayResponseType)
	if err != nil {
		return nil, fmt.Errorf("cruciform: awaiting the phone's reply: %w", err)
	}
	return resp, nil
}
