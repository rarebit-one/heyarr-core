package weblogin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/rarebit-one/voidbind-go/notify"
	"github.com/rarebit-one/voidbind-go/rp"
)

// UnwrapWakePrefix is where the cruciform-offload wake endpoint mounts — POST
// /v1/unwrap-wake. Like /v1/subscriptions it sits OUTSIDE /api/v1 and its bearer
// guard: the request is self-authenticating by the device's enrolment cert
// against the pinned trust set, exactly as a subscription is (ADR-0098, ADR-0055).
const UnwrapWakePrefix = "/v1/unwrap-wake" // #nosec G101 -- a URL path, not a credential

// unwrapWaker serves POST /v1/unwrap-wake: an enrolled device (the offload
// desktop) asks the node to wake ITS user's paired phone for a vault-key unwrap.
// The node fans an opaque voidbind:unwrap?relay=&session= ping to that user's
// subscribed devices (notify.EnqueueUnwrap) — the away-path counterpart to the
// LAN-direct discovery the desktop tries first.
//
// The desktop authenticates with its enrolment cert, the SAME way it authenticates
// to /v1/subscriptions and the same way the login broker verifies an approval: the
// user woken is the cert's user, never a field the client claimed, so a device can
// only wake its own user's phones. The desktop offloads its ENCRYPTION custody to
// the phone but is still an enrolled device with a signing identity, which is what
// this cert proves (ADR-0098): the transport key that authenticates the offload
// exchange itself is a separate, desktop↔phone pairing key and never reaches here.
//
// The ping is opaque by construction (notify.NewUnwrapPing): it carries only the
// public (relay, session) pointer — never a key, a wrapped blob, or a challenge —
// so a node, a push server, or a wake channel learns nothing from relaying it.
type unwrapWaker struct {
	verifier rp.Verifier
	notifier *notify.Notifier
	now      func() time.Time
	log      *slog.Logger
}

// unwrapWakeReq is the wire request: the device's enrolment cert and the
// membership ops it presents (ADR-0007), plus the relay session the phone should
// open for this unwrap. relay_base and session are the SAME opaque pointer the
// desktop posted its signed request into — public, unguessable, no secret.
type unwrapWakeReq struct {
	Cert      string   `json:"cert"`
	Ops       []string `json:"ops,omitempty"`
	RelayBase string   `json:"relay_base"`
	Session   string   `json:"session"`
}

// unwrapWakeResp reports how many of the user's devices were woken — 0 is not an
// error (the phone may be reachable on the LAN, or simply unsubscribed), just as a
// login push to an unsubscribed user wakes nobody.
type unwrapWakeResp struct {
	Woken int `json:"woken"`
}

func (u *unwrapWaker) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

func (u *unwrapWaker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Share the subscription surface's op-budget body cap, and refuse an oversized
	// op set before any verification with the same opaque 401 the cert chain gives.
	var req unwrapWakeReq
	if err := json.NewDecoder(io.LimitReader(r.Body, rp.MaxPresentedOpsBodyBytes)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.Ops) > rp.MaxPresentedOps {
		http.Error(w, "not authorised", http.StatusUnauthorized)
		return
	}
	if req.RelayBase == "" || req.Session == "" {
		http.Error(w, "relay_base and session are required", http.StatusBadRequest)
		return
	}

	// The user woken is the cert's user, never a field the client claimed.
	auth, err := u.verifier.Verify(req.Cert, req.Ops, u.clock())
	if err != nil {
		// Any cert-chain failure (un-enrolled user, bad or expired cert, no trust
		// set) is an opaque 401 — the same stance the subscription surface takes.
		http.Error(w, "not authorised", http.StatusUnauthorized)
		return
	}

	receipt, err := u.notifier.EnqueueUnwrap(r.Context(), notify.EnqueueUnwrapRequest{
		UserID:    auth.UserID,
		RelayBase: req.RelayBase,
		Session:   req.Session,
	})
	if err != nil {
		// The wake plane failed (e.g. the push transport is unreachable); the
		// desktop can still try the LAN-direct path, so this is a bad-gateway, not
		// a hard failure of the request itself.
		if u.log != nil {
			u.log.Warn("offload: waking devices for an unwrap", "user", auth.UserID, "err", err)
		}
		http.Error(w, "wake failed", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(unwrapWakeResp{Woken: receipt.Woken()})
}
