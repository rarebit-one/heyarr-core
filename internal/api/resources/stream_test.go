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

// streamGapEventName mirrors the unexported constant in the package under
// test, so this file can assert a gap is ABSENT.
const streamGapEventName = "heyarr.stream.gap"

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
// A slow consumer must not lose events.
//
// The log drops NOTIFICATIONS for a subscriber that falls behind rather than
// backpressuring whoever is writing (ADR-0009). That used to mean a slow client
// lost events, so the stream reported a gap and closed, and the client had to
// reconnect to recover them.
//
// It no longer does. Every frame is read from the log rather than taken from
// the notification, so a dropped notification costs latency and nothing else,
// and a slow client backpressures its own read instead: the write blocks, the
// drain waits, and the log is untouched. This asserts the stronger property
// that replaced the gap — everything arrives, in order, with no reconnect.
func TestASlowConsumerReceivesEverythingInOrder(t *testing.T) {
	// A one-event subscription buffer, so the notifications are certainly
	// dropped. If delivery depended on them, this test could not pass.
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
	if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
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
	// does and the subscription certainly overflows.
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

	// Now read, slowly and to the end. Every sequence must appear exactly once,
	// in order, and no gap notice may appear at all.
	var (
		seen      []int64
		currentID int64
	)
	for seen == nil || seen[len(seen)-1] < last {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("after %d events: %v", len(seen), err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "id: "):
			currentID, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		case line == "event: "+streamGapEventName:
			t.Fatalf("the stream reported a gap after %d events; a dropped notification is not a "+
				"lost event once delivery comes from the log", len(seen))
		case strings.HasPrefix(line, "data: ") && currentID != 0:
			seen = append(seen, currentID)
			currentID = 0
		}
	}

	if int64(len(seen)) != last {
		t.Fatalf("received %d events, want %d — every sequence from 1 to %d exactly once",
			len(seen), last, last)
	}
	for i, seq := range seen {
		if want := int64(i + 1); seq != want {
			t.Fatalf("event %d was seq %d, want %d — delivery is out of order or has a hole", i, seq, want)
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

// The property #58 was filed for: an event emitted by ANOTHER ROLE reaches an
// open stream without the client reconnecting.
//
// events.Log.publish fans out to that Log's own subscribers, and every role
// constructs its own Log against the same database — even inside `heyarr all`,
// because roles may not share an in-process pointer (ADR-0002). A second Log
// here is exactly what the worker is to the controller: same database, separate
// subscriber set, no way to reach this connection except through the log
// itself.
//
// Before the stream tailed the log, this event was durable immediately and
// invisible until the next reconnect, so `heyarr events tail` could not watch a
// scan happen.
func TestAnEventFromAnotherRoleReachesAnOpenStream(t *testing.T) {
	h := newHarness(t)
	stream := h.openStream("?after=0")
	defer stream.close()

	// Get past the catch-up read FIRST, by emitting locally and waiting for it.
	//
	// Without this the test passes for the wrong reason: openStream returns as
	// soon as the stream reports itself ready, which is before the catch-up
	// loop has finished, so an event emitted immediately afterwards is picked
	// up by the catch-up READ rather than by the live path. Verified — with the
	// poll disabled this test still passed until this handshake existed.
	marker, err := h.events.Emit(t.Context(), events.TypeSystemStarted, "system", "marker", nil)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if f := stream.next(); f.ID >= marker.Seq {
			break
		}
	}

	// A separate Log on the same database: the worker's, as far as this
	// connection is concerned.
	otherRole, err := events.New(events.Options{Writer: h.db.Writer(), Reader: h.db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	if otherRole == h.events {
		t.Fatal("the test built the same Log twice — it would prove nothing")
	}

	emitted, err := otherRole.Emit(t.Context(), events.TypeIngestCompleted, "asset", "asset-from-worker",
		map[string]any{"deduplicated": false})
	if err != nil {
		t.Fatalf("emitting from the other role: %v", err)
	}

	frame := stream.next()
	if frame.ID != emitted.Seq {
		t.Fatalf("received seq %d, want the event the other role emitted (%d)", frame.ID, emitted.Seq)
	}
	if !strings.Contains(frame.Data, "asset-from-worker") {
		t.Errorf("frame does not carry the emitted event: %s", frame.Data)
	}
}

// ...and it must arrive exactly once. The stream both subscribes and polls, so
// an event emitted by THIS role can reach the connection twice unless the two
// paths share a high-water mark.
func TestAnEventIsDeliveredExactlyOnceDespiteBothPaths(t *testing.T) {
	h := newHarness(t)
	stream := h.openStream("?after=0")
	defer stream.close()

	otherRole, err := events.New(events.Options{Writer: h.db.Writer(), Reader: h.db.Reader()})
	if err != nil {
		t.Fatal(err)
	}

	// Emit through both Logs: the local one reaches the subscription AND the
	// poll, the remote one only the poll.
	const each = 4
	want := map[int64]string{}
	for i := range each {
		local, err := h.events.Emit(t.Context(), events.TypeBlobCreated, "blob", fmt.Sprintf("local-%d", i), nil)
		if err != nil {
			t.Fatal(err)
		}
		want[local.Seq] = local.ID
		remote, err := otherRole.Emit(t.Context(), events.TypeBlobCreated, "blob", fmt.Sprintf("remote-%d", i), nil)
		if err != nil {
			t.Fatal(err)
		}
		want[remote.Seq] = remote.ID
	}

	// Fail on the FIRST duplicate rather than waiting for a deadline. A
	// duplicate never adds a new key, so a set-based loop would simply hang and
	// report "nothing arrived", which is true and useless.
	seen := map[int64]int{}
	for len(seen) < len(want) {
		frame := stream.next() // fails the test on its own deadline
		seen[frame.ID]++
		if seen[frame.ID] > 1 {
			t.Fatalf("seq %d was delivered twice — the subscription and the poll are not sharing "+
				"a high-water mark, so an event emitted by this role arrives once from each", frame.ID)
		}
		if _, ok := want[frame.ID]; !ok {
			t.Fatalf("seq %d was delivered but never emitted", frame.ID)
		}
	}
}

// The ordering hazard of having two delivery paths.
//
// The subscription is instant; the poll is not. So a locally-emitted event can
// reach the connection BEFORE an earlier event emitted by another role. If the
// subscription is allowed to advance the high-water mark, the next poll reads
// from a sequence past the earlier event and it is never delivered — silently,
// which is the failure mode this endpoint exists not to have.
//
// Emit remote-then-local with no gap between them, so the local event's
// subscription delivery races ahead of the poll that would have carried the
// remote one.
func TestAnEarlierEventFromAnotherRoleIsNotSkippedByALaterLocalOne(t *testing.T) {
	h := newHarness(t)
	stream := h.openStream("?after=0")
	defer stream.close()

	otherRole, err := events.New(events.Options{Writer: h.db.Writer(), Reader: h.db.Reader()})
	if err != nil {
		t.Fatal(err)
	}

	// Past the catch-up read first, or this tests the catch-up rather than the
	// live path.
	marker, err := h.events.Emit(t.Context(), events.TypeSystemStarted, "system", "marker", nil)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if f := stream.next(); f.ID >= marker.Seq {
			break
		}
	}

	const rounds = 6
	want := make([]int64, 0, rounds*2)
	for i := range rounds {
		remote, err := otherRole.Emit(t.Context(), events.TypeBlobCreated, "blob", fmt.Sprintf("remote-%d", i), nil)
		if err != nil {
			t.Fatal(err)
		}
		local, err := h.events.Emit(t.Context(), events.TypeAssetCreated, "asset", fmt.Sprintf("local-%d", i), nil)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, remote.Seq, local.Seq)
	}

	got := make([]int64, 0, len(want))
	for len(got) < len(want) {
		got = append(got, stream.next().ID)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("frame %d was seq %d, want %d\n  emitted: %v\n  received: %v\n"+
				"an event from another role was skipped or reordered: delivery must be ordered by "+
				"the log, not by whichever path happened to see it first", i, got[i], want[i], want, got)
		}
	}
}
