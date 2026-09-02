// Package weblogin stands up heyarr's Voidbind QR web-login (ADR-0053): the
// browser/TV counterpart to the device-cert scheme (ADR-0048). A browser or a
// television holds no device cert and no device key, so it logs in the
// WhatsApp-Web way — it shows a voidbind:login QR, a device the account owner has
// enrolled approves the challenge, and the broker mints a short-lived session
// token the browser then carries as `Authorization: Bearer <token>`.
//
// It is the heyarr analog of All Thing's ADR-0006 wiring, over the SAME pinned
// trust that backs deviceauth: the login broker's trust set is heyarr's
// user-identity store (internal/deviceauth), so a QR login is honoured for
// exactly the users a device credential is. The heavy lifting — challenge
// minting, offline cert verification, token issuance — is voidbind-go/weblogin;
// this package is the mount and the DB-backed trust adapter.
//
// It mounts on the UNAUTHENTICATED router (like the renderer, relay and the
// Subsonic/OPDS/DLNA adapters), because a browser starting a login has no
// credential to present and the endpoints grant no authority on their own: an
// approval is a hardware-gated Ed25519 signature verified offline against a
// pinned key, and a token is issued only after one lands.
package weblogin

import (
	"context"
	"crypto/ed25519"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/voidbind-go/notify"
	"github.com/rarebit-one/voidbind-go/rp"
	"github.com/rarebit-one/voidbind-go/weblogin"
)

// LoginPrefix is where the broker's JSON API is mounted (POST /login, GET
// /login/{id}, /login/{id}/challenge, POST /login/{id}/approve). Like the relay
// and renderer routes it is deliberately OUTSIDE /api/v1 and its bearer guard.
const LoginPrefix = "/login"

// SigninPath serves the static page that drives the login for a browser.
const SigninPath = "/signin"

//go:embed signin.html
var assets embed.FS

// Handler mounts the QR web-login routes and adapts the broker's minted tokens
// into the httpapi session-authentication seam. It also mounts the push/wake
// plane (ADR-0055): the /v1/subscriptions registry and the login-init wake.
type Handler struct {
	broker    *weblogin.Broker
	base      string
	signin    []byte
	log       *slog.Logger
	subRoutes http.Handler  // notify plane's POST/DELETE /v1/subscriptions
	push      loginNotifier // wakes subscribed devices on a login initiation
}

// Options configure a Handler.
type Options struct {
	// Identities is the pinned user-identity store whose users back the login
	// trust — the SAME pin set deviceauth authenticates device certs against
	// (ADR-0048/0053). Required.
	Identities *deviceauth.Store
	// Base is this node's externally reachable origin (renderBaseURL): embedded in
	// the login QR so the scanning device knows where to fetch the challenge and
	// post its approval, and bound into every challenge as its audience so an
	// approval for this node cannot be replayed at another. Required — a login the
	// scanning device cannot dial back is useless, so the caller declines to mount
	// this at all when it cannot name an origin.
	Base   string
	Logger *slog.Logger

	// NtfyBaseURL records this deployment's self-hosted ntfy origin (ADR-0055). It
	// is informational — a device registers its FULL ntfy topic URL as its
	// subscription endpoint, so the plane works with this empty — and is logged by
	// the caller at startup. Optional.
	NtfyBaseURL string
	// WakeChannel overrides the default ntfy transport (notify.NtfyChannel). A test
	// injects a fake so the push plane is exercised with no live ntfy server; nil
	// selects the shipped ntfy/UnifiedPush channel. Optional.
	WakeChannel notify.WakeChannel
	// SubscriptionStore overrides the default in-memory subscription store
	// (notify.NewMemStore). A durable notify.FileStore may be passed for a
	// deployment that wants the address book to survive a restart; nil uses the
	// in-memory store, which is rebuilt as devices re-register on app open. Optional.
	SubscriptionStore notify.Store
}

// New builds the Handler and its broker.
func New(opts Options) (*Handler, error) {
	if opts.Identities == nil {
		return nil, errors.New("weblogin: a device-identity store is required")
	}
	if strings.TrimSpace(opts.Base) == "" {
		return nil, errors.New("weblogin: an external base origin is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	// The broker evaluates an approving device's membership over the SAME op log
	// the Device scheme does (ADR-0068): a device a member has removed can no
	// longer approve a login, and an approval carrying the ops of a device this
	// node has never met is judged with them. The admin's local tombstone
	// (RevokeDevice) is not an op and is not consulted here — see userTrust.
	broker, err := weblogin.NewBroker(weblogin.BrokerOptions{
		Trust:      userTrust{store: opts.Identities},
		Membership: opts.Identities.Membership(context.Background()),
		Audience:   opts.Base,
	})
	if err != nil {
		return nil, fmt.Errorf("weblogin: building the broker: %w", err)
	}
	page, err := assets.ReadFile("signin.html")
	if err != nil {
		return nil, fmt.Errorf("weblogin: reading the signin page: %w", err)
	}
	log = log.With("component", "weblogin")

	// The push/wake plane (ADR-0055), over the SAME pinned trust the broker uses.
	// It is additive to the QR: the store and channel default to zero-config
	// (in-memory address book, ntfy transport), and a login initiation wakes only
	// devices that have registered a subscription — never the critical path.
	trust := userTrust{store: opts.Identities}
	store := opts.SubscriptionStore
	if store == nil {
		store = notify.NewMemStore()
	}
	channel := opts.WakeChannel
	if channel == nil {
		channel = notify.NtfyChannel{Title: "Heyarr login"}
	}
	notifier := &notify.Notifier{
		Store:    store,
		Channels: map[string]notify.WakeChannel{channel.Name(): channel},
	}
	identities := opts.Identities
	push := &pushNotifier{
		notifier: notifier,
		rpBase:   opts.Base,
		// Resolved on each initiation so a user enrolled or revoked through the API is
		// woken (or not) on the very next login — no static list, no restart. A QR login
		// is user-agnostic at initiation, so every pinned user's devices are addressed;
		// Enqueue is a no-op for any that never subscribed.
		pinned: func(ctx context.Context) ([]string, error) {
			users, err := identities.ListUsers(ctx)
			if err != nil {
				return nil, fmt.Errorf("weblogin: listing pinned users for push: %w", err)
			}
			ids := make([]string, 0, len(users))
			for _, u := range users {
				ids = append(ids, u.PublicKey)
			}
			return ids, nil
		},
		log: log,
	}

	return &Handler{
		broker:    broker,
		base:      opts.Base,
		signin:    page,
		log:       log,
		subRoutes: SubscriptionRoutes(store, trust, opts.Identities.Membership(context.Background()), nil),
		push:      push,
	}, nil
}

// Mount registers the login routes and the /signin page on an unauthenticated
// router. It is an httpapi.MountFunc, mounted alongside the other public
// handlers.
//
// The broker's own ServeMux (Go 1.22 method+wildcard patterns) sees the full
// request path, so it does the method routing for /login and its sub-paths; chi
// only dispatches the prefix to it.
func (h *Handler) Mount(r chi.Router) {
	// The broker's login routes, wrapped so a successful POST /login also wakes the
	// pinned users' subscribed devices (ADR-0055). The wrap is a passthrough for
	// every other request, including the /login/{id} sub-routes.
	routes := loginInitPush((&weblogin.Handler{Broker: h.broker, Base: h.base}).Routes(), h.push, h.log)
	r.Handle(LoginPrefix, routes)
	r.Handle(LoginPrefix+"/*", routes)
	r.Get(SigninPath, h.handleSignin)
	// The notify plane's device-facing registry (POST/DELETE /v1/subscriptions),
	// cert-authenticated by the plane itself against the same pinned trust.
	r.Handle(SubscriptionsPrefix, h.subRoutes)
}

func (h *Handler) handleSignin(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(h.signin)
}

// Sessions returns the httpapi.SessionValidator that accepts this broker's minted
// tokens as a Bearer credential on heyarr's authenticated routes. It is handed to
// httpapi.Options.SessionValidator, closing the loop: a token the QR login minted
// here is the credential the /api/v1 guard honours.
func (h *Handler) Sessions() httpapi.SessionValidator { return brokerSessions{h.broker} }

// brokerSessions adapts a weblogin.Broker to httpapi.SessionValidator: a token is
// live iff the broker still holds a session for it, resolved back to the pinned
// principal the broker minted it for (the user whose device approved the login).
type brokerSessions struct{ b *weblogin.Broker }

func (s brokerSessions) Session(token string) (httpapi.SessionPrincipal, bool) {
	a, ok := s.b.ValidateToken(token)
	if !ok {
		return httpapi.SessionPrincipal{}, false
	}
	return httpapi.SessionPrincipal{UserID: a.UserID, DeviceKey: a.DeviceKey}, true
}

// userTrust adapts heyarr's DB-backed user-identity store to rp.TrustStore, the
// pinned-user set the broker verifies a login cert against. It is the same
// resolution deviceauth.Verify does per request (look the pinned user up, parse
// its key), narrowed to the one question rp asks: "is this user pinned, and under
// which key". A user the store does not know is refused (ok=false → the broker
// reports rp.ErrUnknownUser): enrol before trust, exactly as for a device cert.
//
// Note (ADR-0053, ADR-0068): this pins on the USER, like All Thing's rp trust.
// The broker's membership evaluation now honours a member-signed REMOVE, so a
// device removed through the op log cannot approve a login; what it still does
// not consult is the admin's local tombstone (deviceauth.RevokeDevice), which is
// not an op, so an admin-revoked device whose user is still pinned can approve
// a QR login until its user is unpinned or a member removes it. The token it
// mints is short-lived and read-only; binding weblogin approval to the tombstone
// is a tracked follow-up.
type userTrust struct{ store *deviceauth.Store }

func (t userTrust) PinnedUserKey(userID string) (ed25519.PublicKey, bool) {
	u, err := t.store.LookupUser(context.Background(), userID)
	if err != nil {
		return nil, false
	}
	pub, err := identity.ParsePublicKey(u.PublicKey)
	if err != nil {
		return nil, false
	}
	return pub, true
}

// compile-time assertions the adapters satisfy the seams they are handed to.
var (
	_ rp.TrustStore            = userTrust{}
	_ httpapi.SessionValidator = brokerSessions{}
)
