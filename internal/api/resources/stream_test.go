// Every HTTP response in this file is closed either by the harness's t.Cleanup
// or by the stream reader goroutine, neither of which bodyclose can see
// through.
//
//nolint:bodyclose // responses are closed by the harness and the reader goroutine
package resources_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// sseFrame is one parsed event-stream frame.
type sseFrame struct {
	ID   int64
	Name string
	Data string
}

// sseConn is a live connection to GET /api/v1/events.
type sseConn struct {
	t      *testing.T
	cancel context.CancelFunc
	frames chan sseFrame
	done   chan struct{}
}

// open connects and blocks until the stream says it is ready.
//
// The ready comment is the readiness *condition*, not a proxy for it: it is
// written after the subscription is registered, so a test that has seen it
// knows an event emitted next cannot be missed. Sleeping instead — or waiting
// on "the request returned" — is how four tests in this repo have failed on CI.
func (h *harness) openStream(query string) *sseConn {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.http.URL+"/api/v1/events"+query, nil)
	if err != nil {
		cancel()
		h.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the reader goroutine
	if err != nil {
		cancel()
		h.t.Fatalf("opening the stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		h.t.Fatalf("the stream returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		cancel()
		h.t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	c := &sseConn{t: h.t, cancel: cancel, frames: make(chan sseFrame, 4096), done: make(chan struct{})}
	ready := make(chan struct{})
	go func() {
		defer close(c.done)
		defer func() { _ = resp.Body.Close() }()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var frame sseFrame
		readySeen := false
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == ": ready":
				if !readySeen {
					readySeen = true
					close(ready)
				}
			case line == "":
				if frame.Name != "" || frame.Data != "" {
					select {
					case c.frames <- frame:
					default:
					}
				}
				frame = sseFrame{}
			case strings.HasPrefix(line, "id: "):
				n, _ := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
				frame.ID = n
			case strings.HasPrefix(line, "event: "):
				frame.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				frame.Data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		cancel()
		h.t.Fatal("the stream never said it was ready")
	}
	h.t.Cleanup(cancel)
	return c
}

// next waits for one frame.
func (c *sseConn) next() sseFrame {
	c.t.Helper()
	select {
	case f := <-c.frames:
		return f
	case <-time.After(10 * time.Second):
		c.t.Fatal("no event arrived on the stream within the deadline")
		return sseFrame{}
	}
}

// take waits for n frames.
func (c *sseConn) take(n int) []sseFrame {
	c.t.Helper()
	out := make([]sseFrame, 0, n)
	for range n {
		out = append(out, c.next())
	}
	return out
}

func (c *sseConn) close() {
	c.cancel()
	<-c.done
}

// emit appends an event and returns its sequence number.
func (h *harness) emit(eventType, subject string) int64 {
	h.t.Helper()
	e, err := h.events.Emit(context.Background(), eventType, "blob", subject, map[string]string{"subject": subject})
	if err != nil {
		h.t.Fatal(err)
	}
	return e.Seq
}

// Reconnection has to be gapless: a client that saw sequence N asks for
// everything after N, gets the backlog, and switches to live without a hole and
// without a duplicate.
//
// The stream is killed mid-flight rather than closed politely, because a
// polite close is not the case that breaks — a dropped connection is, and it
// drops at whatever point the server happened to have reached.
func TestReconnectingTheEventStreamIsGapless(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Events that exist before anyone connects must come out of the log.
	var want []int64
	for i := range 5 {
		want = append(want, h.emit(events.TypeBlobCreated, fmt.Sprintf("pre-%d", i)))
	}

	first := h.openStream("?after=0")
	// Live events, arriving while the connection is open.
	for i := range 5 {
		want = append(want, h.emit(events.TypeBlobCreated, fmt.Sprintf("live-%d", i)))
	}

	got := []int64{}
	// Read part of the stream, then kill it mid-flight.
	for _, f := range first.take(7) {
		got = append(got, f.ID)
	}
	first.close()

	// Events emitted while nothing is connected are the ones a polling client
	// loses and this one must not.
	for i := range 4 {
		want = append(want, h.emit(events.TypeBlobCreated, fmt.Sprintf("gap-%d", i)))
	}

	highest := got[len(got)-1]
	for _, seq := range got {
		if seq > highest {
			highest = seq
		}
	}

	second := h.openStream("?after=" + strconv.FormatInt(highest, 10))
	remaining := len(want) - len(got)
	for _, f := range second.take(remaining) {
		got = append(got, f.ID)
	}
	// And it is still live after the catch-up.
	want = append(want, h.emit(events.TypeBlobCreated, "after-reconnect"))
	got = append(got, second.next().ID)
	second.close()

	// Also verify against the log itself, so the test cannot pass by both
	// halves agreeing on a wrong answer.
	logged, err := h.events.Since(ctx, 0, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(logged) != len(want) {
		t.Fatalf("the log holds %d events and the test emitted %d", len(logged), len(want))
	}

	assertNoGapsOrDuplicates(t, want, got)
}

// assertNoGapsOrDuplicates is the whole point of the SSE tests, so it says
// exactly which sequence numbers were lost or repeated rather than "not equal".
func assertNoGapsOrDuplicates(t *testing.T, want, got []int64) {
	t.Helper()
	seen := map[int64]int{}
	for _, seq := range got {
		seen[seq]++
	}
	var duplicated, missing []int64
	for _, seq := range want {
		switch seen[seq] {
		case 1:
		case 0:
			missing = append(missing, seq)
		default:
			duplicated = append(duplicated, seq)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the stream had a hole: sequences %v were never delivered", missing)
	}
	if len(duplicated) > 0 {
		t.Errorf("the stream delivered %v more than once", duplicated)
	}
	if len(got) != len(want) {
		t.Errorf("the stream delivered %d events; %d were emitted", len(got), len(want))
	}
}

// A slow subscriber is dropped rather than backpressured (ADR-0009), and the
// client has to be able to tell that happened. A stream that silently continues
// with a hole in it is worse than one that closes, because the client believes
// it is current.
//
// The connection is a raw socket with a deliberately tiny receive buffer, so
// that the server really does block on the write and the subscription really
// does overflow. Flooding an ordinary socket and hoping the kernel buffer fills
// is a test that passes on one machine and not the next.
func TestTheStreamSaysSoWhenItHasDroppedEvents(t *testing.T) {
	h := newHarness(t, withStreamBuffer(1))

	addr := strings.TrimPrefix(h.http.URL, "http://")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.SetReadBuffer(2048); err != nil {
			t.Fatalf("shrinking the receive buffer: %v", err)
		}
	}
	if _, err := fmt.Fprintf(conn, "GET /api/v1/events?after=0 HTTP/1.1\r\nHost: %s\r\n\r\n", addr); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(conn)
	// Read up to and including the ready comment, then stop reading entirely.
	// From here the server can only write as much as the socket will hold.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the stream preamble: %v", err)
		}
		if strings.TrimRight(line, "\r\n") == ": ready" {
			break
		}
	}

	// Each event carries a kilobyte, so the socket fills long before the log
	// does. The subscription holds one event; everything past that is dropped.
	payload := strings.Repeat("x", 1024)
	const flood = 300
	var last int64
	for i := range flood {
		e, err := h.events.Emit(context.Background(), events.TypeBlobCreated, "blob",
			fmt.Sprintf("flood-%03d", i), map[string]string{"filler": payload})
		if err != nil {
			t.Fatal(err)
		}
		last = e.Seq
	}

	// Now drain. The server unblocks, notices the drop, tells us where to
	// resume and closes.
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var gap struct {
		ResumeAfter int64  `json:"resume_after"`
		Dropped     int64  `json:"dropped"`
		Detail      string `json:"detail"`
	}
	found := false
	var delivered []int64
	var currentID int64
	for !found {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "id: "):
			currentID, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		case line == "event: heyarr.stream.gap":
			found = true
		case strings.HasPrefix(line, "data: ") && !found && currentID != 0:
			delivered = append(delivered, currentID)
			currentID = 0
		}
	}
	if !found {
		t.Fatalf("the stream delivered %d of %d events and never said it had dropped any; "+
			"a client would believe it was current", len(delivered), flood)
	}
	// Read the gap payload.
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("the gap event had no data line: %v", err)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimRight(line, "\r\n"), "data: ")), &gap); err != nil {
		t.Fatalf("the gap event's data is not JSON: %v", err)
	}
	if gap.Dropped == 0 {
		t.Error("the gap event reports zero dropped events, which is not a gap")
	}
	if gap.ResumeAfter == 0 {
		t.Error("the gap event does not say where to resume from, which is the only thing it is for")
	}
	if gap.Detail == "" {
		t.Error("the gap event does not say what happened")
	}

	// The recovery has to actually work: reconnecting where it said picks up
	// everything that was dropped.
	recovered := h.openStream("?after=" + strconv.FormatInt(gap.ResumeAfter, 10))
	defer recovered.close()
	missing := int(last - gap.ResumeAfter)
	if missing <= 0 {
		t.Fatalf("resume_after (%d) is at or past the last event (%d), so nothing was actually recovered",
			gap.ResumeAfter, last)
	}
	seqs := []int64{}
	for _, f := range recovered.take(missing) {
		seqs = append(seqs, f.ID)
	}
	for i, seq := range seqs {
		if want := gap.ResumeAfter + int64(i) + 1; seq != want {
			t.Fatalf("the refill delivered %d where %d was expected — resuming did not close the gap", seq, want)
		}
	}
}

// ?types= is what makes the stream usable for `heyarr scan --wait`: a client
// that has to receive every event in the system to notice its own job finishing
// is a client that falls behind and gets dropped.
func TestTheStreamFiltersByType(t *testing.T) {
	h := newHarness(t)

	h.emit(events.TypeBlobCreated, "before-blob")
	jobBefore := h.emit(events.TypeJobEnqueued, "before-job")

	c := h.openStream("?after=0&types=job.*")
	defer c.close()

	h.emit(events.TypeBlobCreated, "live-blob")
	jobLive := h.emit(events.TypeJobSucceeded, "live-job")

	got := c.take(2)
	if got[0].ID != jobBefore || got[1].ID != jobLive {
		t.Fatalf("the filtered stream delivered %v; want the two job events %d and %d",
			[]int64{got[0].ID, got[1].ID}, jobBefore, jobLive)
	}
	for _, f := range got {
		if !strings.HasPrefix(f.Name, "job.") {
			t.Errorf("a %s event came through a job.* filter", f.Name)
		}
	}
}

// The frames have to be what an EventSource expects, or the browser client that
// this endpoint exists for silently receives nothing.
func TestTheStreamIsWellFormedSSE(t *testing.T) {
	h := newHarness(t)
	c := h.openStream("?after=0")
	defer c.close()

	seq := h.emit(events.TypeBlobCreated, "well-formed")
	f := c.next()
	if f.ID != seq {
		t.Errorf("the frame id is %d, want the sequence number %d — a browser resumes from this", f.ID, seq)
	}
	if f.Name != events.TypeBlobCreated {
		t.Errorf("the frame event name is %q, want the event type %q", f.Name, events.TypeBlobCreated)
	}
	var e events.Event
	if err := json.Unmarshal([]byte(f.Data), &e); err != nil {
		t.Fatalf("the frame data is not an event: %v", err)
	}
	if e.Seq != seq || e.Type != events.TypeBlobCreated || e.SubjectID != "well-formed" {
		t.Errorf("the frame data does not describe the event that was emitted: %+v", e)
	}
}

// Last-Event-ID is how a browser resumes on its own. Ignoring it would mean the
// one client type that reconnects automatically is the one that reconnects with
// a gap.
func TestTheStreamHonoursLastEventID(t *testing.T) {
	h := newHarness(t)
	first := h.emit(events.TypeBlobCreated, "one")
	second := h.emit(events.TypeBlobCreated, "two")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.http.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", strconv.FormatInt(first, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id: ") {
			got, _ := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			if got != second {
				t.Errorf("the first delivered event is %d; Last-Event-ID was %d so it must be %d",
					got, first, second)
			}
			return
		}
	}
	t.Fatal("the stream delivered nothing, so Last-Event-ID was not honoured")
}

func TestTheStreamRejectsNonsense(t *testing.T) {
	h := newHarness(t)
	for _, q := range []string{"?after=yesterday", "?after=-4"} {
		resp := h.get("/api/v1/events" + q)
		raw := h.body(resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /api/v1/events%s = %d, want 400: %s", q, resp.StatusCode, raw)
			continue
		}
		decodeProblem(t, resp, raw)
	}
}

// The ordering of Subscribe and the catch-up read is the whole gapless
// property, and it is invisible from the outside: both orders look identical on
// a quiet system, and the wrong one loses exactly the events committed between
// the read finishing and the subscription starting.
//
// So it is asserted directly rather than by timing. The ready comment is
// written after Subscribe, so at the instant a client sees it the log must
// already have a subscriber. Reversing the two in the handler makes the log
// report none here, because in that version the handler is still inside a
// database query when ready goes out.
//
// This test exists because the obvious version — emit and check nothing is
// lost — passes with the two reversed: the catch-up loop re-queries until it
// is exhausted, so it swallows anything a test can emit quickly enough to
// matter. A test that cannot fail is a comment.
func TestTheStreamSubscribesBeforeItSaysItIsReady(t *testing.T) {
	h := newHarness(t)

	// A backlog large enough that the catch-up read is several queries. In a
	// handler that subscribed afterwards, this is the window.
	for i := range 600 {
		h.emit(events.TypeBlobCreated, fmt.Sprintf("backlog-%03d", i))
	}

	if n := h.events.SubscriberCount(); n != 0 {
		t.Fatalf("the log already had %d subscribers before the stream opened", n)
	}

	c := h.openStream("?after=0")
	defer c.close()

	if n := h.events.SubscriberCount(); n != 1 {
		t.Fatalf("the stream reported itself ready with %d subscribers on the log; "+
			"ready must mean subscribed, or every event committed between the catch-up "+
			"read and the subscription is lost with nothing to show for it", n)
	}
}
