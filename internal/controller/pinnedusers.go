package controller

import (
	"context"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
)

// warnIfNoUsersPinned logs, once at startup, that a controller which
// authenticates requests has no user pinned, so every device credential it is
// shown will be refused as device_unknown_user (#802).
//
// Pins are per-controller (ADR-0038): a device whose user is pinned at one site
// is a stranger at another until the user is pinned there too. The caller gets
// the same opaque 401 any bad credential gets — deliberately, see
// clockWindowHint — so the refusal says nothing on the wire, and without this
// line the only trace on the operator's side is a stream of rejections. The
// warning is the operator-facing half; nothing network-observable changes.
//
// With authentication off there is nothing to refuse, so it stays quiet. A
// failed read is logged and startup carries on: this is a diagnostic, and a
// controller should not refuse to start over one.
func warnIfNoUsersPinned(ctx context.Context, log *slog.Logger, authEnabled bool, store *deviceauth.Store) {
	if !authEnabled {
		return
	}
	users, err := store.ListUsers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("could not count pinned users", "error", err)
		}
		return
	}
	if len(users) > 0 {
		return
	}
	log.Warn(noUsersPinnedWarning,
		"refusal", "device_unknown_user",
		"fix", "pin each user with `heyarr admin user pin` on every controller its devices authenticate at")
}

// noUsersPinnedWarning is the startup line's message, a constant so the test
// and an operator's log filter match the same text.
const noUsersPinnedWarning = "no users pinned on this controller; every device credential will be refused"
