package weblogin

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/void-which-binds-go/enrolment"
	"github.com/rarebit-one/void-which-binds-go/notify"
	"github.com/rarebit-one/void-which-binds-go/rp"
)

// UnwrapWakePrefix is where the cruciform-offload wake endpoint mounts — POST
// /v1/unwrap-wake. Like /login it sits OUTSIDE /api/v1 and its bearer guard: the
// request is self-authenticating by the device's enrolment cert against the
// pinned trust set (ADR-0098).
const UnwrapWakePrefix = "/v1/unwrap-wake" // #nosec G101 -- a URL path, not a credential

// unwrapWaker serves POST /v1/unwrap-wake: an enrolled device (the offload
// desktop) asks the node to wake ITS user's paired phone for a vault-key unwrap.
// The node asks the shared notify plane (ADR-0102) to fan an opaque
// voidbind:unwrap?relay=&session= ping to that user's subscribed devices
// (POST /v1/enqueue-unwrap) — the away-path counterpart to the LAN-direct
// discovery the desktop tries first. The desktop never holds the plane's bearer;
// the node does, and it spends it only for a device it has authenticated.
//
// The desktop authenticates with its enrolment cert, the same way the login
// broker verifies an approval: the user woken is the cert's user, never a field
// the client claimed, so a device can only wake its own user's phones. The desktop offloads its ENCRYPTION custody to
// the phone but is still an enrolled device with a signing identity, which is what
// this cert proves (ADR-0098): the transport key that authenticates the offload
// exchange itself is a separate, desktop↔phone pairing key and never reaches here.
//
// The cert is paired with a possession proof for it — the `possession` field, or
// `cert` as the Device credential `<cert>~<proof>`, the two spellings the
// notify plane's registry also takes — and the request is authenticated check →
// possession → commit (rp.Verifier.VerifyWithPossession, voidbind-go#70), so
// the membership ops it presents are recorded only once the caller has proved
// it holds the device key. A BARE cert (the pre-v0.18 wire) is still served for
// compatibility, but it is only CHECKED — nothing it presents is recorded — and
// the device is logged once as still to migrate. (That compatibility is for
// heyarr desktops older than v0.5.3 (#685), which predate the library's v0.18
// possession proofs, not for phones, so it outlives the removed
// /v1/subscriptions registry; see barecert.go.)
//
// The ping is opaque by construction (notify.NewUnwrapPing): it carries only the
// public (relay, session) pointer — never a key, a wrapped blob, or a challenge —
// so a node, a push server, or a wake channel learns nothing from relaying it.
type unwrapWaker struct {
	verifier rp.Verifier
	// enqueuer is the notify plane; nil means no plane is configured, and an
	// authenticated request then wakes nobody (200, woken 0) — exactly the answer a
	// user with no subscribed phone gets, so the desktop still polls the relay for
	// a phone that is already reachable.
	enqueuer notify.UnwrapEnqueuer
	now      func() time.Time
	log      *slog.Logger
	bare     *bareCertWarner
}

// unwrapWakeReq is the wire request: the device's enrolment cert and the
// membership ops it presents (ADR-0007), plus the relay session the phone should
// open for this unwrap. relay_base and session are the SAME opaque pointer the
// desktop posted its signed request into — public, unguessable, no secret.
type unwrapWakeReq struct {
	Cert string `json:"cert"`
	// Possession is the device's possession proof for Cert
	// (enrolment.SignPossession), unless Cert already carries it as
	// `<cert>~<proof>`. Optional for now: see authenticate.
	Possession string   `json:"possession,omitempty"`
	Ops        []string `json:"ops,omitempty"`
	RelayBase  string   `json:"relay_base"`
	Session    string   `json:"session"`
}

// unwrapWakeResp reports how many of the user's devices were woken — 0 is not an
// error (the phone may be reachable on the LAN, simply unsubscribed, or this node
// has no notify plane configured), just as a login push to an unsubscribed user
// wakes nobody.
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
	// Cap the body at the presented-op budget, and refuse an oversized op set
	// before any verification with the same opaque 401 the cert chain gives.
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
	auth, err := u.authenticate(req.Cert, req.Possession, req.Ops, u.clock())
	if err != nil {
		// Any cert-chain failure (un-enrolled user, bad or expired cert, no trust
		// set) is an opaque 401.
		http.Error(w, "not authorised", http.StatusUnauthorized)
		return
	}

	if u.enqueuer == nil {
		writeWoken(w, 0)
		return
	}
	receipt, err := u.enqueuer.EnqueueUnwrap(r.Context(), notify.EnqueueUnwrapRequest{
		UserID:    auth.UserID,
		RelayBase: req.RelayBase,
		Session:   req.Session,
	})
	if err != nil {
		// The wake plane failed (unreachable, bearer refused, rate-limited); the
		// desktop can still try the LAN-direct path, so this is a bad-gateway, not
		// a hard failure of the request itself. The plane's error never names the
		// bearer, and the desktop is told only that the wake failed.
		if u.log != nil {
			u.log.Warn("offload: waking devices for an unwrap", "user", auth.UserID, "err", err)
		}
		http.Error(w, "wake failed", http.StatusBadGateway)
		return
	}

	writeWoken(w, receipt.Woken())
}

// writeWoken answers an authenticated unwrap-wake with how many devices woke.
func writeWoken(w http.ResponseWriter, woken int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(unwrapWakeResp{Woken: woken})
}

// authenticate is the check → possession → commit chain (voidbind-go#70). With a
// proof it is rp.Verifier.VerifyWithPossession: the presented ops are persisted
// only once the proof verifies. Without one the cert is only checked — it must
// still name a current member, since that is what picks the user woken — and
// nothing it presents is recorded, because a bare cert may be a replay.
func (u *unwrapWaker) authenticate(cert, possession string, ops []string, now time.Time) (rp.Authenticated, error) {
	cert, possession = strings.TrimSpace(cert), strings.TrimSpace(possession)
	if cred, embedded, found := strings.Cut(cert, enrolment.CredentialSeparator); found {
		if possession != "" {
			return rp.Authenticated{}, errPossessionTwice
		}
		cert, possession = strings.TrimSpace(cred), strings.TrimSpace(embedded)
	}
	if possession != "" {
		return u.verifier.VerifyWithPossession(cert, possession, ops, now)
	}
	pending, err := u.verifier.Check(cert, ops, now)
	if err != nil {
		return rp.Authenticated{}, err
	}
	if u.bare != nil {
		u.bare.warn(pending.Authenticated)
	}
	return pending.Authenticated, nil
}

// errPossessionTwice is a request that carried a proof both in `possession` and
// embedded in `cert`. Like every other authentication failure it answers the
// opaque 401.
var errPossessionTwice = errors.New("weblogin: possession proof given twice")
