package client

// Device-authenticated transport (ADR-0048, ADR-0032, #329).
//
// A device does not hold a bearer token. It authenticates by proving, per
// request, that it holds the device key a user vouched for: its enrolment cert
// joined to a FRESH possession proof it signs on the spot. The proof lives only
// enrolment.PossessionTTL (two minutes) by design — it IS the replay window — so
// a client that cached the credential would present a stale string that the peer
// 401s as `possession_expired` almost immediately. The whole point of this
// transport is that it never caches: it mints a new proof for every HTTP request
// through the shared http.RoundTripper, which is exactly where "one request now,
// another minutes later" each gets its own fresh mint.

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/enrolment"
)

// DeviceScheme is the HTTP Authorization scheme a device presents, the
// counterpart to "Bearer". It matches deviceauth.Scheme on the server; it is
// spelled here rather than imported so the CLI client does not drag the
// control-plane store (and SQLite) into every binary that only wants to call a
// peer.
const DeviceScheme = "Device"

// Credentialer mints the value a device presents under the Device scheme: its
// held enrolment cert joined to a FRESH possession proof signed with the device
// key (enrolment.SignPossession, via the device store). *device.Store satisfies
// it, and the CLI passes one.
//
// now is the instant the proof is minted for and ttl its lifetime (zero means
// enrolment.PossessionTTL). It is called once per request by the transport, so
// the proof presented is never older than the request that carries it. The
// device key is loaded and consumed inside the implementation and never returned
// — no caller holds the key merely to authenticate.
type Credentialer interface {
	Credential(now time.Time, ttl time.Duration) (string, error)
}

// maxDeviceReMints bounds the graceful re-mint on a clock-skew refusal. A laptop
// that wakes from sleep with a drifted clock can sign a proof the peer reads as
// not_yet_valid or expired (the ADR-0048 asymmetry). The peer discloses that one
// fact, and only that one, in a `WWW-Authenticate: Device error="…"` header
// (#420), because it is derivable from the caller's own watch and leaks nothing
// about identity. On it the client re-mints, nudging its issue time toward the
// peer's clock, and retries — never failing hard on skew (docs/design/
// mobile-client.md §2). Everything else — a revoked device, an unpinned user — is
// an opaque 401 the client must not retry, so only the two clock refusals here do.
const maxDeviceReMints = 4

// deviceAuthTransport injects a freshly minted Device credential per request.
type deviceAuthTransport struct {
	base http.RoundTripper
	cred Credentialer
	// now is the client clock, injectable so a demo or test can advance it past
	// PossessionTTL and prove the second request is a fresh mint, not a cached
	// string. Nil is never stored; New installs time.Now.
	now func() time.Time
}

// RoundTrip mints a fresh possession proof, sets the Device header, and sends
// the request — re-minting on a clock-skew refusal up to maxDeviceReMints.
//
// It obeys the RoundTripper contract: it does not mutate the caller's request
// (it works on a clone) and it retries only a request whose body it can rewind
// (GetBody present, or no body at all — the whole of the browse surface --peer
// serves is GET). A body it cannot replay is sent once and its response, skew
// refusal or not, returned as-is.
func (t *deviceAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	replayable := req.Body == nil || req.GetBody != nil
	var skew time.Duration
	for attempt := 0; ; attempt++ {
		clone := req.Clone(req.Context())
		if req.Body != nil && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("device auth: rewinding the request body to re-sign: %w", err)
			}
			clone.Body = body
		}
		cred, err := t.cred.Credential(t.now().Add(skew), 0)
		if err != nil {
			return nil, fmt.Errorf("device auth: minting a possession proof: %w", err)
		}
		clone.Header.Set("Authorization", DeviceScheme+" "+cred)

		resp, err := t.base.RoundTrip(clone)
		if err != nil {
			return nil, err
		}

		hint := clockSkewHint(resp)
		if resp.StatusCode != http.StatusUnauthorized || hint == "" ||
			attempt >= maxDeviceReMints || !replayable {
			return resp, nil
		}
		// A clock-skew refusal, and we can retry: discard this response, nudge the
		// mint time in the direction the peer named, and sign again.
		drainAndClose(resp)
		skew = nudgeSkew(skew, hint)
	}
}

// nudgeSkew moves the client's mint time toward the peer's clock by one
// possession-skew step in the named direction. `not_yet_valid` means this
// machine's clock is ahead of the peer's (the proof is issued in the peer's
// future), so the next proof is issued earlier; `expired` means it is behind (or
// the proof aged in flight), so the next is issued later. The step is
// enrolment.PossessionSkew, the peer's own tolerance, so a moderate drift
// converges within maxDeviceReMints and a hopeless one gives up cleanly rather
// than looping.
func nudgeSkew(skew time.Duration, hint string) time.Duration {
	switch hint {
	case "not_yet_valid":
		return skew - enrolment.PossessionSkew
	case "expired":
		return skew + enrolment.PossessionSkew
	default:
		return skew
	}
}

// clockSkewHint reads the one recoverable fact the peer discloses about a
// rejected device credential: whether the possession proof (or cert) was
// not_yet_valid or expired against the peer's clock. It is the value of the
// `error` auth-param on a `Device` challenge (RFC 7235 shape). Any other refusal,
// and any non-401, returns "" — there is nothing to retry.
func clockSkewHint(resp *http.Response) string {
	if resp.StatusCode != http.StatusUnauthorized {
		return ""
	}
	for _, h := range resp.Header.Values("WWW-Authenticate") {
		if !strings.HasPrefix(h, DeviceScheme+" ") {
			continue
		}
		switch {
		case strings.Contains(h, `error="not_yet_valid"`):
			return "not_yet_valid"
		case strings.Contains(h, `error="expired"`):
			return "expired"
		}
	}
	return ""
}

// deviceAuthAdvice turns the peer's deliberately opaque 401 into a sentence a
// person can act on. The peer refuses a revoked device and an unpinned user
// identically on purpose (internal/api/http/auth.go): the difference is free
// reconnaissance, so the client cannot and must not claim to know which it was.
// What it CAN say is the short list of things the operator can check, and — when
// the peer disclosed a clock skew — that this machine's clock is the thing to
// fix. Returns "" for any response that is not a device-auth rejection.
func deviceAuthAdvice(resp *http.Response) string {
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		return ""
	}
	if hint := clockSkewHint(resp); hint != "" {
		return fmt.Sprintf("the peer rejected this device's possession proof as %s even after re-minting: "+
			"this machine's clock is too far from the peer's for the two-minute possession window. "+
			"Sync this machine's clock and retry.", hint)
	}
	return "the peer rejected this device's credential. It may be revoked there, or your user identity " +
		"may not be pinned at this peer yet (ADR-0032): pin the identity's public key at the peer, or " +
		"check that this device has not been revoked."
}

// drainAndClose releases a response whose body will not be read, so the
// connection can be reused for the retry.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_ = resp.Body.Close()
}
