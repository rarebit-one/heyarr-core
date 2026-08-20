package resources

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// GET /api/v1/events is the external event transport (§76, ADR-0009).
//
// The property that matters is gapless reconnection: a client that saw seq N
// reconnects with ?after=N and receives everything since, with no hole and no
// duplicate. Everything below exists to make that true rather than
// approximately true:
//
//   - The subscription is opened BEFORE the catch-up read. The other order
//     leaves a window — everything committed between the read finishing and the
//     subscription starting is in neither, and it is invisible, because both
//     halves look complete on their own.
//   - The catch-up read therefore overlaps the live feed. Live events at or
//     below the sequence the catch-up reached are dropped, which is the only
//     place a duplicate could come from.
//   - A slow consumer is dropped by the event log rather than backpressured
//     (ADR-0009), so the stream reports the drop and closes instead of quietly
//     continuing with a hole in it. A client that is told to reconnect recovers
//     everything from the log; a client that is told nothing does not.

// streamGapEvent is the SSE event name used to tell a client its stream lost
// events. It is namespaced away from §76's event types so it can never collide
// with a real one.
const streamGapEvent = "heyarr.stream.gap"

// backlogBatch bounds one catch-up read. A client reconnecting after a week
// must not make the server materialise a week of events in one slice.
const backlogBatch = 500

// subscriberBuffer is how many events may queue for one connection before the
// log gives up on it. Large enough that an ordinary client never notices a
// burst, small enough that a stalled one is detected in bounded memory.
const subscriberBuffer = 256

func (a *API) streamEvents(w http.ResponseWriter, r *http.Request) {
	after, err := streamAfter(r)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	types, err := streamTypes(r)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	// Subscribe first. See the comment above: the reverse order has a silent
	// window, which is the failure mode this endpoint exists to not have.
	sub := a.events.Subscribe(a.buffer, types...)
	defer sub.Close()

	flusher, ok := w.(interface{ Flush() })
	if !ok {
		// Nothing downstream can stream, so say so rather than buffering the
		// whole stream in memory and delivering it when the client gives up.
		a.log.Error("the response writer cannot flush, so SSE is not possible",
			"request_id", httpapi.RequestIDFrom(r.Context()))
		httpapi.Fail(w, r, problem.Internal())
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Tells nginx not to buffer the stream. Without it a reverse proxy turns an
	// event stream into a very slow file download and the symptom is "events
	// arrive in bursts of 4 KB", which nobody diagnoses quickly.
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	s := &stream{w: w, flusher: flusher.Flush}
	// The retry hint and the ready comment go out before anything else, and the
	// ready comment is load-bearing for tests as well as clients: it is the
	// point at which the subscription definitely exists, so a test can wait for
	// the condition instead of sleeping and hoping.
	s.raw("retry: 3000\n\n")
	s.raw(": ready\n\n")
	s.flush()

	// Catch up from the log.
	catchUpTo := after
	for {
		batch, err := a.events.Since(r.Context(), catchUpTo, types, backlogBatch)
		if err != nil {
			a.log.Error("reading the event backlog failed",
				"request_id", httpapi.RequestIDFrom(r.Context()), "after", catchUpTo, "error", err)
			// The headers are already out, so there is no honest way to turn
			// this into a problem document. Telling the client to reconnect is
			// the next best thing.
			s.gap(catchUpTo, 0, "the event backlog could not be read")
			return
		}
		for _, e := range batch {
			if !s.event(e) {
				return
			}
			catchUpTo = e.Seq
		}
		if len(batch) < backlogBatch {
			break
		}
	}

	ticker := time.NewTicker(a.heartbeat)
	defer ticker.Stop()

	maxSeq := catchUpTo
	for {
		select {
		case <-r.Context().Done():
			return

		case e, open := <-sub.Events():
			if !open {
				return
			}
			// Already delivered by the catch-up read. This is the one place a
			// duplicate could enter the stream.
			if e.Seq <= catchUpTo {
				continue
			}
			if !s.event(e) {
				return
			}
			if e.Seq > maxSeq {
				maxSeq = e.Seq
			}
			if n := sub.Dropped(); n > 0 {
				s.gap(maxSeq, n, "this connection fell behind and events were dropped")
				return
			}

		case <-ticker.C:
			// A drop while the stream is idle must still be reported, or a
			// client sits on a silent connection believing it is current.
			if n := sub.Dropped(); n > 0 {
				s.gap(maxSeq, n, "this connection fell behind and events were dropped")
				return
			}
			s.raw(": heartbeat\n\n")
			if !s.flush() {
				return
			}
		}
	}
}

// stream writes SSE frames and remembers the first write error, so a client
// that has gone away stops the handler rather than being written to forever.
type stream struct {
	w       http.ResponseWriter
	flusher func()
	broken  bool
}

func (s *stream) raw(text string) {
	if s.broken {
		return
	}
	// #nosec G705 -- SSE frames are served as text/event-stream with nosniff,
	// and every value interpolated into one is either a number or JSON from
	// encoding/json. There is no HTML context here.
	if _, err := s.w.Write([]byte(text)); err != nil {
		s.broken = true
	}
}

// flush pushes what has been written to the client and reports whether the
// connection is still usable.
func (s *stream) flush() bool {
	if s.broken {
		return false
	}
	s.flusher()
	return true
}

// event writes one §76 event. The SSE id is the sequence number, so a browser
// EventSource reconnecting sends Last-Event-ID and resumes exactly where
// ?after= would have.
func (s *stream) event(e events.Event) bool {
	body, err := marshal(e)
	if err != nil {
		// An event whose payload will not marshal must not silently vanish from
		// the stream; skipping it would be an invisible gap.
		body = []byte(fmt.Sprintf(`{"seq":%d,"type":%q,"error":"payload could not be encoded"}`,
			e.Seq, e.Type))
	}
	s.raw("id: " + strconv.FormatInt(e.Seq, 10) + "\n")
	s.raw("event: " + e.Type + "\n")
	s.raw("data: " + string(body) + "\n\n")
	return s.flush()
}

// gap tells the client its view is incomplete and where to resume from. The
// alternative — closing the connection silently — is indistinguishable from a
// network blip, and a client cannot tell "reconnect and refill" from "carry on".
func (s *stream) gap(resumeAfter, dropped int64, detail string) {
	s.raw("event: " + streamGapEvent + "\n")
	s.raw(fmt.Sprintf(`data: {"resume_after":%d,"dropped":%d,"detail":%q}`+"\n\n",
		resumeAfter, dropped, detail))
	s.flush()
}

// streamAfter reads the resume point from ?after=, falling back to the
// Last-Event-ID header that a browser EventSource sends on its own reconnect.
func streamAfter(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("after")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("after must be a non-negative sequence number, not %q", raw)
	}
	return n, nil
}

// streamTypes parses ?types=, which accepts exact §76 type names and trailing-*
// namespace prefixes.
func streamTypes(r *http.Request) ([]string, error) {
	raw := r.URL.Query().Get("types")
	if raw == "" {
		return nil, nil
	}
	var out []string
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if strings.ContainsAny(t, " \t\n") {
			return nil, fmt.Errorf("types must be a comma-separated list of event types, not %q", raw)
		}
		out = append(out, t)
	}
	return out, nil
}
