package mtls

import (
	"fmt"
	"net/http"
	"time"
)

// DialTimeout bounds a peer handshake.
//
// A refusal is a failed handshake rather than a status code, so the only way a
// caller learns it was refused is that the dial returns an error. Without a
// deadline a peer that accepts the connection and then says nothing is
// indistinguishable from one that is merely slow, and "refused" becomes "hung"
// — which is the one outcome a test cannot tell from a pass.
const DialTimeout = 15 * time.Second

// Client is an HTTP client that speaks only to pinned peers.
//
// It is the only supported way to make a peer request. A caller that built its
// own http.Client would be one line away from an unpinned transport, and the
// resulting traffic would look identical until the day it mattered.
func Client(opts Options) (*http.Client, error) {
	cfg, err := ClientConfig(opts)
	if err != nil {
		return nil, err
	}
	// Asserted again at the point of use, not because pinned() might have
	// failed to do its job, but because this is the seam a future caller will
	// reach for when it wants "the same thing but with one field changed".
	if err := AssertPinned(cfg); err != nil {
		return nil, fmt.Errorf("mtls: building a peer client: %w", err)
	}
	return &http.Client{
		Timeout: DialTimeout,
		Transport: &http.Transport{
			TLSClientConfig:     cfg,
			TLSHandshakeTimeout: DialTimeout,
			// One connection per peer, kept alive. Membership is consulted on
			// every request as well as every handshake (M4-04), so reuse costs
			// nothing in freshness — and a pool that reconnected constantly
			// would make "revocation severs an already-open connection"
			// untestable, because there would rarely be one.
			MaxIdleConnsPerHost: 1,
			ForceAttemptHTTP2:   true,
		},
	}, nil
}
