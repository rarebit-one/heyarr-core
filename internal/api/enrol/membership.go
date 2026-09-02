package enrol

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/voidbind-go/rp"
)

// The membership routes (ADR-0068). An identity's device set is evaluated from
// a set of signed ops that any of its members may have issued, so a node and
// its devices have to be able to EXCHANGE ops: a device learns from the node
// that a member removed it (or another device), and a device tells the node
// about a remove — or an admission — it made while the node was not in the
// loop. Both are public: an op carries its own authority (a member's
// signature, judged by evaluation against the pinned genesis key), the node
// adds none by accepting it, and the device pushing a remove may be one that
// no longer authenticates.

// Membership is the GET /membership/{usr} response, and the POST response
// after recording: every op token this node holds for the identity, oldest
// issued first. A device merges them with its own (enrolment.Merge) and
// evaluates; the node does not pre-chew a view for it, because a view is a
// function of (ops, clock) and the device has its own clock.
type Membership struct {
	User string   `json:"usr"`
	Ops  []string `json:"ops"`
	// Recorded is how many of the posted ops this node had not seen; absent on
	// a GET.
	Recorded *int `json:"recorded,omitempty"`
}

// pushRequest is the POST /membership/{usr} body.
type pushRequest struct {
	Ops []string `json:"ops"`
}

func (h *Handler) getMembership(w http.ResponseWriter, r *http.Request) {
	usr, ok := h.pinnedUser(w, r)
	if !ok {
		return
	}
	ops, err := h.identities.Ops(r.Context(), usr)
	if err != nil {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	h.writeMembership(w, http.StatusOK, Membership{User: usr, Ops: nonNil(ops)})
}

// postMembership records ops a device pushes. It evaluates them merged with
// the log first and records only what evaluation accepted as structurally
// valid for THIS identity; a body in which any op is junk — unparseable, a bad
// signature, another identity's, citing a past nobody can see — is refused as
// a whole, opaquely (the same 401 and detail the Device scheme gives, the real
// reason in the log under its bounded label set), so a probe learns nothing
// about which of its ops the node could read. An op that is valid but
// ineffective (unauthorised signer, outranked, a re-add of a removed device)
// IS recorded: it is part of the state and citable, and evaluation is what
// decides that it changes nothing.
func (h *Handler) postMembership(w http.ResponseWriter, r *http.Request) {
	usr, ok := h.pinnedUser(w, r)
	if !ok {
		return
	}
	var body pushRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if len(body.Ops) == 0 {
		httpapi.Fail(w, r, problem.BadRequest(`"ops" must carry at least one membership op`))
		return
	}
	if len(body.Ops) > rp.MaxPresentedOps {
		httpapi.Fail(w, r, problem.BadRequest("at most "+itoa(rp.MaxPresentedOps)+" membership ops may be pushed at once"))
		return
	}
	stored, err := h.identities.Ops(r.Context(), usr)
	if err != nil {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	view, err := enrolment.Evaluate(usr, enrolment.Merge(stored, body.Ops), h.identities.Now())
	if err != nil {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	known := make(map[string]struct{}, len(stored))
	for _, tok := range stored {
		known[enrolment.OpHash(tok)] = struct{}{}
	}
	var fresh []string
	for _, tok := range body.Ops {
		hash := enrolment.OpHash(tok)
		if reason, rejected := view.Rejected[hash]; rejected {
			h.log.Warn("rejected a membership push",
				"request_id", httpapi.RequestIDFrom(r.Context()),
				"reason", "membership_op_"+string(reason))
			httpapi.Fail(w, r, problem.Unauthorized(rejectedDetail))
			return
		}
		if _, seen := known[hash]; seen {
			continue
		}
		known[hash] = struct{}{}
		fresh = append(fresh, tok)
	}
	if err := h.identities.RecordOps(r.Context(), usr, fresh); err != nil {
		if errors.Is(err, deviceauth.ErrMalformedOp) || errors.Is(err, deviceauth.ErrUnknownUser) {
			h.log.Warn("rejected a membership push",
				"request_id", httpapi.RequestIDFrom(r.Context()),
				"reason", httpapi.DeviceFailureReason(err))
			httpapi.Fail(w, r, problem.Unauthorized(rejectedDetail))
			return
		}
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if len(fresh) > 0 {
		h.log.Info("recorded pushed membership ops",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"user", usr, "count", len(fresh))
	}
	ops, err := h.identities.Ops(r.Context(), usr)
	if err != nil {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	n := len(fresh)
	h.writeMembership(w, http.StatusOK, Membership{User: usr, Ops: nonNil(ops), Recorded: &n})
}

// pinnedUser resolves the {usr} path segment: a rendered Ed25519 key that this
// node has pinned. A key that does not parse is a 400 (a paste error); one
// that parses but is not pinned is a 404 — a device learns it is talking to a
// node that does not know its identity, which is the actionable fact, and a
// public key is not guessable, so there is nothing to enumerate.
func (h *Handler) pinnedUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := chi.URLParam(r, "usr")
	pub, err := identity.ParsePublicKey(raw)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest("usr must be a rendered ed25519 public key"))
		return "", false
	}
	usr := identity.FormatPublicKey(pub)
	if _, err := h.identities.LookupUser(r.Context(), usr); err != nil {
		if errors.Is(err, deviceauth.ErrUnknownUser) {
			httpapi.Fail(w, r, problem.NotFound("no such identity is pinned here"))
			return "", false
		}
		httpapi.Fail(w, r, problem.Internal())
		return "", false
	}
	return usr, true
}

func (h *Handler) writeMembership(w http.ResponseWriter, status int, m Membership) {
	out, err := json.Marshal(m)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// nonNil renders an empty op list as [] rather than null: a client iterating
// the list should not have to special-case "no ops yet".
func nonNil(ops []string) []string {
	if ops == nil {
		return []string{}
	}
	return ops
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
