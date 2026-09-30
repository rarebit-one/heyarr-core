package weblogin

import (
	"log/slog"
	"sync"

	"github.com/rarebit-one/void-which-binds-go/rp"
)

// bareCertWarner logs, once per device per process, a device that authenticated
// to a cert-authed public route (/v1/subscriptions, /v1/unwrap-wake) with a BARE
// enrolment cert — no possession proof.
//
// A cert is a public token: anyone who saw one could present it. Since
// void-which-binds-go v0.18 (voidbind-go#70) such a request is still served,
// because deployed clients send exactly that, but nothing it presents is recorded
// to the membership log; only a request with a verified proof teaches this node
// new ops. The warning is the migration list: once no device appears in it, the
// bare-cert path can be refused outright.
type bareCertWarner struct {
	log   *slog.Logger
	route string
	seen  sync.Map // device key → struct{}
}

func newBareCertWarner(log *slog.Logger, route string) *bareCertWarner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &bareCertWarner{log: log, route: route}
}

// warn records a bare-cert request from auth's device. It is safe for concurrent
// use, as notify.Registry.OnBareCert requires.
func (w *bareCertWarner) warn(auth rp.Authenticated) {
	if _, loaded := w.seen.LoadOrStore(auth.DeviceKey, struct{}{}); loaded {
		return
	}
	w.log.Warn("deprecated: device authenticated with a bare cert and no possession proof; its presented ops were not recorded",
		"route", w.route, "user", auth.UserID, "device", auth.DeviceKey)
}
