package weblogin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/rarebit-one/voidbind-go/notify"
	"github.com/rarebit-one/voidbind-go/rp"
)

// This file wires heyarr as a relying party of Voidbind's push/wake plane
// (voidbind-go/notify, v0.5.0) — the push counterpart to the QR web-login broker
// (ADR-0053/0055). It adds two things behind the SAME pinned trust the broker and
// the device-cert scheme already use (internal/deviceauth):
//
//   - The subscription registry (POST/DELETE /v1/subscriptions): an ENROLLED
//     device registers the ntfy wake endpoint its phone listens on. notify.Handler
//     authenticates every registration with the device's enrolment cert against
//     the pinned user set (rp.TrustStore — heyarr's userTrust), so only an enrolled
//     device may subscribe and the subscription is bound to the authenticated
//     (user, device), never to fields the client claimed.
//   - A wake on login INITIATION: a successful POST /login also pushes the opaque
//     voidbind:login?rp=&id= ping to the subscribed devices of the pinned users, so
//     a phone can approve without the browser's QR being scanned. The QR stays the
//     primary channel — a push failure or an unsubscribed user never blocks the
//     login (push is additive and fail-open).
//
// The ping is opaque by construction (notify.NewPing → weblogin.EncodeLogin): it
// carries ONLY the public (rp, id) tuple, byte-identical to the QR, never a cert,
// a challenge, a match number, or any secret. See TestPushPingIsOpaque.
//
// It mirrors All Thing's ADR-0009 wiring (the first relying party of this plane),
// with one adaptation: All Thing is single-user and pins a static user list at
// construction, whereas heyarr resolves the pinned users from its device-identity
// store on each initiation, so a user enrolled or revoked through the API is woken
// (or not) on the very next login without a restart.

// SubscriptionsPrefix is where the notify plane's device-facing routes mount —
// POST/DELETE /v1/subscriptions. Like /login it is OUTSIDE /api/v1 and its bearer
// guard: the notify.Handler authenticates each request with the device's
// enrolment cert against the pinned trust set itself, exactly as the login broker
// verifies an approval offline.
const SubscriptionsPrefix = "/v1/subscriptions"

// loginNotifier wakes paired devices when a QR web-login is initiated. It is the
// seam loginInitPush calls; pushNotifier is the production implementation and a
// test supplies a fake to assert an initiation fired (or did not) a wake.
type loginNotifier interface {
	// NotifyLogin fans the opaque ping for loginID to the paired devices of the
	// users it addresses, returning the number of devices woken. A user with no
	// live subscription wakes zero devices and is NOT an error — the initiating
	// browser simply falls back to showing the QR.
	NotifyLogin(ctx context.Context, loginID string) (woken int, err error)
}

// pushNotifier fans a login's opaque ping to the subscribed devices of the pinned
// users it resolves, over their wake channels (notify.Notifier). A QR login is
// user-agnostic at initiation (any pinned device may approve), so heyarr wakes
// every pinned user's paired devices; Enqueue is a no-op for any user who never
// registered a subscription, which is exactly "only push to users who registered".
type pushNotifier struct {
	// notifier is the voidbind-go fan-out half of the push plane (required). Its
	// Store is the subscription address book and its Channels map a channel kind
	// (notify.ChannelNtfy) to a transport.
	notifier *notify.Notifier
	// rpBase is this relying party's externally reachable origin — the `rp=` of the
	// pushed tuple, byte-identical to the QR the browser shows.
	rpBase string
	// pinned resolves the current pinned user ids ("ed25519:<hex>") to address. It
	// is read on each initiation so runtime enrolment/revocation is honoured; a
	// resolution error is fail-open (log, wake nobody), never a blocked login.
	pinned func(ctx context.Context) ([]string, error)
	log    *slog.Logger
}

// NotifyLogin implements loginNotifier. It is best-effort and fail-open: a wake
// error for one user does not stop the others, and the caller never blocks the
// login on it (the QR remains the fallback).
func (p *pushNotifier) NotifyLogin(ctx context.Context, loginID string) (int, error) {
	if p == nil || p.notifier == nil {
		return 0, nil
	}
	users, err := p.pinned(ctx)
	if err != nil {
		return 0, err
	}
	var (
		woken    int
		firstErr error
	)
	for _, u := range users {
		n, err := p.notifier.Enqueue(ctx, notify.EnqueueRequest{
			UserID:  u,
			RPBase:  p.rpBase,
			LoginID: loginID,
		})
		woken += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return woken, firstErr
}

// SubscriptionRoutes returns the notify plane's device-facing HTTP surface —
// POST/DELETE /v1/subscriptions — bound to the given subscription store, the
// RP's pinned trust set and its membership op log (ADR-0068). Registration and
// unsubscription are authenticated by the device's admitting op, evaluated
// against trust and membership (the plane's own Registry does the verify), so
// this reuses the exact trust set and op log that back the device authenticator
// and the login broker.
//
// now supplies the verification clock; nil means time.Now (production). It is
// injectable so a test can pin the verification instant against a
// frozen-window op.
func SubscriptionRoutes(store notify.Store, trust rp.TrustStore, membership rp.Membership, now func() time.Time) http.Handler {
	h := &notify.Handler{Registry: notify.Registry{Store: store, Trust: trust, Membership: membership, Now: now}}
	return h.Routes()
}

// loginInitPush wraps the weblogin routes so a successful login initiation (POST
// /login) ALSO wakes the pinned users' paired devices via push. Every other
// request — a different method, or any /login/{id} sub-route — is passed through
// untouched.
//
// The create response is small machine JSON, so the wrapper fully BUFFERS it (a
// throwaway recorder), reads the minted login id, then relays the buffered
// response to the browser verbatim and fires the wake: push is additive to the
// QR, never on its critical path. The relay asserts a non-HTML content type and
// `nosniff` so the broker's id/QR — which trace from the request — can never be
// interpreted as markup by a browser (defence in depth; the response was already
// JSON).
//
// A nil notifier returns next unchanged, so a node with no push plane is exactly
// the unwrapped weblogin handler.
func loginInitPush(next http.Handler, n loginNotifier, log *slog.Logger) http.Handler {
	if n == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != LoginPrefix {
			next.ServeHTTP(w, r)
			return
		}
		// Buffer the whole create response before the browser sees any of it.
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)

		// Relay it verbatim. The create response is JSON, never HTML: pin the content
		// type to a non-renderable one with a string literal (which also lets static
		// analysis see this body is not an XSS sink) and set nosniff, then copy the
		// other headers the broker wrote.
		for k, vs := range rec.Header() {
			if k == "Content-Type" {
				continue
			}
			w.Header()[k] = vs
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())

		if rec.Code != http.StatusOK {
			return // create failed; nothing to wake for
		}
		id := loginIDFromBody(rec.Body.Bytes())
		if id == "" {
			return
		}
		// Best-effort: a wake error never surfaces to the browser (the QR the
		// relayed response already carried is the fallback). r.Context() is still
		// live because ServeHTTP has returned into this same goroutine.
		if _, err := n.NotifyLogin(r.Context(), id); err != nil && log != nil {
			log.Warn("push: waking devices for login", "login", id, "err", err)
		}
	})
}

// loginIDFromBody pulls the "id" field out of a weblogin create response
// (`{"id":"...","qr":"...",...}`). A body that does not parse or carries no id
// yields "" — loginInitPush then simply does not push, never erroring.
func loginIDFromBody(b []byte) string {
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return ""
	}
	return resp.ID
}
