package weblogin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/rarebit-one/void-which-binds-go/notify"
)

// This file wires heyarr's push login to the SHARED Voidbind notify plane
// (void-which-binds-notify, ADR-0102) — the push counterpart to the QR web-login
// broker (ADR-0053). A successful POST /login also asks the plane to push the
// opaque voidbind:login?rp=&id= ping to the paired devices of the pinned users, so
// a phone can approve without the browser's QR being scanned. The QR stays the
// primary channel — a push failure, an unconfigured plane or an unsubscribed user
// never blocks the login (push is additive and fail-open).
//
// heyarr holds NO subscription registry of its own. A phone subscribes to exactly
// one notify base, the shared plane, so a registry embedded here would never gain
// a subscriber (void-which-binds-go#86). heyarr only calls the plane's
// bearer-gated POST /v1/enqueue with rp_base set to itself; the plane, not heyarr,
// builds the ping.
//
// The ping is opaque by construction (notify.NewPing → weblogin.EncodeLogin): it
// carries ONLY the public (rp, id) tuple, byte-identical to the QR, never a cert,
// a challenge, a match number, or any secret — and heyarr sends the plane nothing
// more than the pinned user id, its own base and the login id.
//
// heyarr resolves the pinned users from its device-identity store on each
// initiation, so a user enrolled or revoked through the API is woken (or not) on
// the very next login without a restart.

// loginPushTimeout bounds the whole fan-out for one login initiation, across every
// pinned user. The browser already has its QR by then; this only stops a wedged
// plane from holding a request goroutine open.
const loginPushTimeout = 15 * time.Second

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

// pushNotifier asks the notify plane to wake the subscribed devices of the pinned
// users it resolves. A QR login is user-agnostic at initiation (any pinned device
// may approve), so heyarr addresses every pinned user; the plane wakes nobody for
// a user who never subscribed, which is exactly "only push to users who
// registered".
type pushNotifier struct {
	// enqueuer is the plane (a notify.EnqueueClient in production; required).
	enqueuer notify.LoginEnqueuer
	// rpBase is this relying party's externally reachable origin — the `rp=` of the
	// pushed tuple, byte-identical to the QR the browser shows.
	rpBase string
	// pinned resolves the current pinned user ids ("ed25519:<hex>") to address. It
	// is read on each initiation so runtime enrolment/revocation is honoured; a
	// resolution error is fail-open (log, wake nobody), never a blocked login.
	pinned func(ctx context.Context) ([]string, error)
}

// NotifyLogin implements loginNotifier. It is best-effort and fail-open: a wake
// error for one user does not stop the others, and the caller never blocks the
// login on it (the QR remains the fallback). A refused or disabled enqueue bearer
// is the same answer for every user, so it stops the fan-out at the first one.
func (p *pushNotifier) NotifyLogin(ctx context.Context, loginID string) (int, error) {
	if p == nil || p.enqueuer == nil {
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
		n, err := p.enqueuer.Enqueue(ctx, notify.EnqueueRequest{
			UserID:  u,
			RPBase:  p.rpBase,
			LoginID: loginID,
		})
		woken += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if errors.Is(err, notify.ErrEnqueueUnauthorized) || errors.Is(err, notify.ErrEnqueueDisabled) {
			break
		}
	}
	return woken, firstErr
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
// The relayed response is flushed BEFORE the wake, so the browser has its QR
// while the plane is still being called; the wake then runs under its own bound
// (loginPushTimeout), detached from the browser's cancellation so a browser that
// has already read its QR and gone does not abort the push it is waiting on.
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
		// The browser's QR must not wait on the plane. A writer that cannot flush
		// (none in production) just delivers on return, as before.
		_ = http.NewResponseController(w).Flush()

		if rec.Code != http.StatusOK {
			return // create failed; nothing to wake for
		}
		id := loginIDFromBody(rec.Body.Bytes())
		if id == "" {
			return
		}
		// Best-effort: a wake error never surfaces to the browser (the QR the
		// relayed response already carried is the fallback).
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), loginPushTimeout)
		defer cancel()
		if _, err := n.NotifyLogin(ctx, id); err != nil && log != nil {
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
