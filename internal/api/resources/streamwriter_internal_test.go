package resources

import (
	"net/http"
	"testing"
	"time"
)

// deadlineRecorder is a ResponseWriter that records the write deadline the stream
// writer arms, so the stall-timeout wiring is asserted without waiting for it.
type deadlineRecorder struct {
	header   http.Header
	deadline time.Time
	flushed  int
}

func (d *deadlineRecorder) Header() http.Header {
	if d.header == nil {
		d.header = http.Header{}
	}
	return d.header
}
func (d *deadlineRecorder) Write(p []byte) (int, error)        { return len(p), nil }
func (d *deadlineRecorder) WriteHeader(int)                    {}
func (d *deadlineRecorder) Flush()                             { d.flushed++ }
func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error { d.deadline = t; return nil }

// TestTheStreamWriterArmsAStallDeadline guards the stuck-reader leak: a client whose
// connection stays open but drains nothing must not pin ffmpeg. Every write arms a
// fresh deadline, so a slow-but-live client is never cut, and one that stops reading
// trips it and frees the slot.
func TestTheStreamWriterArmsAStallDeadline(t *testing.T) {
	rec := &deadlineRecorder{}
	sw := &streamWriter{w: rec, rc: http.NewResponseController(rec)}

	before := time.Now()
	if _, err := sw.Write([]byte("frag-1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rec.deadline.IsZero() {
		t.Fatal("no write deadline was armed — a stuck client would pin ffmpeg forever")
	}
	if got := rec.deadline.Sub(before); got < streamStallTimeout-time.Second || got > streamStallTimeout+time.Second {
		t.Errorf("deadline %v out, want ~%v", got, streamStallTimeout)
	}

	first := rec.deadline
	time.Sleep(2 * time.Millisecond)
	if _, err := sw.Write([]byte("frag-2")); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if !rec.deadline.After(first) {
		t.Errorf("a second write must re-arm the deadline (a slow client is not cut): %v then %v", first, rec.deadline)
	}
	if rec.flushed < 2 {
		t.Errorf("expected a flush per write, got %d", rec.flushed)
	}
}
