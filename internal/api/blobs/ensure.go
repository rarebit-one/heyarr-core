package blobs

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// TransferEnsurer starts the transfer that fetches a blob this node DESIRES but
// does not hold, so a client GET can block-then-serve off it as bytes land (§33,
// #371 Option A). It is optional on the handler and, like PartialSource, wired
// only by the CLIENT surface: the peer content route leaves it nil and keeps its
// untouched whole-blob contract (ADR-0042).
//
// # It carries the gate, not just the enqueue
//
// EnsureTransfer answers "is this blob desired?" AND, only if so, starts the
// transfer — as one call, because the two are one decision. A GET must never be
// a way to make a node fetch arbitrary content by asking for it (the DoS #371
// names), so the enqueue happens ONLY behind a want/replica intent that already
// exists. `desired=false` means nobody wants this hash: the route starts nothing
// and answers 404 exactly as it did before ensure-on-GET existed.
//
// # It goes through the job table, and that is invariant 4
//
// The transfer is enqueued as a replicate_blob job, not run in-process: the API
// role never reaches into the worker, and the job table is the only channel
// between them. The enqueue is idempotent (invariant 9) — keyed on blob +
// destination, the same key reconciliation uses — so concurrent GETs, and a GET
// racing a reconcile cycle, collapse to one transfer rather than stacking.
type TransferEnsurer interface {
	// EnsureTransfer reports whether the blob is desired and, when it is,
	// idempotently ensures a transfer for it is enqueued. A false report means
	// the gate refused: nothing was started.
	EnsureTransfer(ctx context.Context, blob hashing.Hash) (desired bool, err error)
}

// retryAfterSeconds is the hint a "still fetching" 503 carries. Short, because
// the condition it reports resolves on network timescales: the transfer is
// running and the caller should ask again soon, not back off for minutes.
const retryAfterSeconds = 5

// ensureAndServe starts a transfer for a DESIRED-but-absent blob and blocks
// until it can serve, and reports whether it took the response (§33, #371).
//
// It returns false only when there is nothing to do here — no Ensure capability
// (the peer route), a non-GET (a HEAD asks "do you have it" and must not start a
// fetch), or the gate refusing because the blob is not desired — so the caller
// falls through to the ordinary 404. Once it decides to act it owns the
// response: a served blob, a served partial, a "still fetching" 503, or an
// internal error, but never a misleading "not found".
//
// # The bound, and why it sits only on the pre-serve wait
//
// A GET that started a transfer must not hang if the source never delivers
// (#371). So the wait for the blob to become servable — whole, or in flight as a
// partial — is bounded by ensureTimeout, and a deadline reached before then
// answers 503 "still fetching" with a Retry-After. But the moment the blob IS
// servable the handoff uses the REQUEST's own context, never the bounded one: a
// healthy large transfer streams for as long as it legitimately takes, and only
// a transfer that produced nothing at all is ever cut off. A whole-pull, which
// exposes no partial to stream, is served on completion; until then the client
// sees a bounded 503 it retries, not an open connection that never answers.
func (h *Handler) ensureAndServe(w http.ResponseWriter, r *http.Request, hash hashing.Hash, mime string) bool {
	if h.ensurer == nil {
		return false
	}
	if r.Method != http.MethodGet {
		// A HEAD (the only other method mounted) is a question about what is held,
		// not an instruction to fetch. Starting a transfer for one would make a
		// player's availability check pull the whole library. Fall through to 404.
		return false
	}

	desired, err := h.ensurer.EnsureTransfer(r.Context(), hash)
	if err != nil {
		h.log.Error("ensuring a transfer for a blob failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"hash", hash.String(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return true
	}
	if !desired {
		// THE GATE held: nobody desires this hash, nothing was started, and the
		// caller answers 404. This is what keeps ensure-on-GET from becoming
		// fetch-on-request (#371).
		return false
	}

	// A transfer is ensured. Wait for it to become servable, bounded.
	deadline, cancel := context.WithTimeout(r.Context(), h.ensureTimeout)
	defer cancel()
	for {
		// 1. Completed whole? Serve it as an ordinary finished blob, on the
		//    request's own context so streaming is never bounded by the ensure
		//    deadline. This also covers a small blob a whole-pull finished before
		//    any partial record existed.
		rsc, _, err := h.store.Open(r.Context(), hash)
		switch {
		case err == nil:
			h.serveWhole(w, r, rsc, hash, mime)
			return true
		case !errors.Is(err, cas.ErrNotFound):
			h.log.Error("opening a blob failed",
				"request_id", httpapi.RequestIDFrom(r.Context()),
				"hash", hash.String(), "error", err)
			httpapi.Fail(w, r, problem.Internal())
			return true
		}

		// 2. In flight as a partial? Hand off to M10's block-then-serve, which
		//    sizes the response from the arriving length and streams landed ranges
		//    (§33, §84, ADR-0044) on the request's own context.
		if h.serveIfPartial(w, r, hash, mime) {
			return true
		}

		// 3. Neither yet: the worker has not begun producing bytes. Wait one poll
		//    interval, bounded by the ensure deadline. A deadline reached first is
		//    the "source unavailable / still fetching" case — answer 503 rather
		//    than block forever.
		if err := h.wait(deadline); err != nil {
			if r.Context().Err() != nil {
				// The client left. Nothing to serve and nothing to say.
				return true
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
			httpapi.Fail(w, r, problem.ServiceUnavailable(
				"this peer is fetching blob "+hash.String()+" but it has not arrived yet; retry shortly"))
			return true
		}
	}
}

// serveWhole streams a finished blob and closes its reader. Extracted so the
// ensure path serves a completed transfer through exactly the identity, caching
// and range contract the ordinary whole-blob path uses — the same ETag, the same
// zero modtime, the same ServeContent — with the close handled once.
func (h *Handler) serveWhole(w http.ResponseWriter, r *http.Request, rsc cas.ReadSeekCloser, hash hashing.Hash, mime string) {
	defer func() {
		if err := rsc.Close(); err != nil {
			h.log.Warn("closing a blob failed", "hash", hash.String(), "error", err)
		}
	}()
	h.writeContentHeaders(w, r, hash, mime)
	var noModTime time.Time
	http.ServeContent(w, r, "", noModTime, rsc)
}
