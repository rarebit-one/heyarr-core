// Package enrol serves POST /enrol — a paired device enrolling itself at a peer
// on the strength of its own credential (ADR-0067).
//
// After pairing (ADR-0022; the Voidbind relay, ADR-0066) a phone holds a cert
// the user signed for its key. Before this route the peer honoured that cert
// only after an ADMIN posted it to /api/v1/identities/devices — a CLI or curl
// step in the middle of what should be one tap. The cert already proves the
// pinned user vouched for the device, and a possession proof proves the caller
// holds the key; together they are exactly what the Device scheme verifies on
// every request. So the route verifies them the same way, through the same
// code (deviceauth.Store.SelfEnrol), and records the device.
//
// It adds no authority. ADR-0032's enrol-before-trust gate is the USER pin,
// and it holds: a cert from a user this peer has not pinned is refused. The
// enrolled device authenticates at the read floor; write remains an admin's
// management authorisation of the device key (ADR-0065).
//
// It is fail-closed and opaque, the stance of the authenticate middleware: a
// refusal for any reason — unknown user, bad signature, expired cert, a proof
// by the wrong key or over a different cert, a revoked device — is the same
// 401 with the same detail the Device scheme returns, and the real reason goes
// to the log under the bounded label set that scheme uses. Only a request that
// is not even a credential (not JSON, too large, unknown fields) is a 400.
package enrol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
)

// maxRequestBody bounds the body. A cert is a few hundred bytes and a proof
// under two hundred; 16 KiB is generous headroom and refuses anything abusive,
// on a route that, being unauthenticated, must not buffer what it is sent.
const maxRequestBody = 16 << 10

// rejectedDetail is the one thing a refused caller learns — the same sentence
// the authenticate middleware uses, so the two routes cannot be told apart by
// their refusals either.
const rejectedDetail = "the presented credential was rejected"

// Options configure a Handler.
type Options struct {
	// Identities is the pinned user / enrolled device store — the SAME one the
	// Device scheme verifies against, so a self-enrolment is judged by the rule it
	// will authenticate under. Required.
	Identities *deviceauth.Store
	Logger     *slog.Logger
}

// Handler serves POST /enrol.
type Handler struct {
	identities *deviceauth.Store
	log        *slog.Logger
}

// New builds the Handler.
func New(opts Options) (*Handler, error) {
	if opts.Identities == nil {
		return nil, errors.New("enrol: a device-identity store is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Handler{identities: opts.Identities, log: log.With("component", "enrol")}, nil
}

// Mount registers the route on an unauthenticated router (an httpapi.MountFunc).
func (h *Handler) Mount(r chi.Router) {
	r.Post(httpapi.EnrolPath, h.handle)
}

// request is the POST /enrol body: the user-signed cert, a fresh possession
// proof over it (enrolment.SignPossession — the same proof the Device scheme
// takes after the "~"), and a display name for the device.
type request struct {
	Cert  string `json:"cert"`
	Proof string `json:"proof"`
	Name  string `json:"name"`
}

// Enrolled is the response: the device as the peer now knows it. It is the same
// shape /api/v1/identities/devices renders, so a client has one device record
// to understand.
type Enrolled struct {
	DeviceKey     string    `json:"device_key"`
	EncryptionKey string    `json:"encryption_key,omitempty"`
	Name          string    `json:"name"`
	User          string    `json:"user"`
	EnrolledAt    time.Time `json:"enrolled_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func (h *Handler) handle(w http.ResponseWriter, r *http.Request) {
	var body request
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "device"
	}
	res, err := h.identities.SelfEnrol(r.Context(), body.Cert, body.Proof, name)
	if err != nil {
		h.log.Warn("rejected a self-enrolment",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"reason", httpapi.DeviceFailureReason(err))
		httpapi.Fail(w, r, problem.Unauthorized(rejectedDetail))
		return
	}
	device, user := res.Device, res.User
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
		h.log.Info("device self-enrolled",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"device_key", device.DeviceKey, "user", user.PublicKey, "name", device.Name)
	}
	w.Header().Set("Location", httpapi.APIPrefix+"/identities/devices/"+device.DeviceKey)
	out, err := json.Marshal(Enrolled{
		DeviceKey:     device.DeviceKey,
		EncryptionKey: device.EncryptionKey,
		Name:          device.Name,
		User:          user.PublicKey,
		EnrolledAt:    device.EnrolledAt,
		ExpiresAt:     device.ExpiresAt,
	})
	if err != nil {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// decodeJSON reads exactly one JSON object into v, bounded and strict — the
// resources API's rule, restated here because this route sits outside it.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mediaType, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(mediaType) != "application/json" {
			return fmt.Errorf("the request body must be application/json, not %s", mediaType)
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return fmt.Errorf("the request body is larger than %d bytes", maxRequestBody)
		}
		if errors.Is(err, io.EOF) {
			return errors.New("the request body is empty")
		}
		return fmt.Errorf("the request body is not valid JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("the request body must contain exactly one JSON object")
	}
	return nil
}
