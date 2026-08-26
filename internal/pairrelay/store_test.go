package pairrelay

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestPutGetRoundTrip(t *testing.T) {
	s := New(Options{})
	if err := s.Put("sess-1", "initiator_commit", []byte("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get("sess-1", "initiator_commit")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
	if _, err := s.Get("sess-1", "cert"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an empty slot should be ErrNotFound, got %v", err)
	}
}

func TestWriteOnceIdempotentButConflictsOnDifferentBytes(t *testing.T) {
	s := New(Options{})
	if err := s.Put("s", "cert", []byte("A")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("s", "cert", []byte("A")); err != nil {
		t.Fatalf("a repeat of the same bytes should be idempotent: %v", err)
	}
	if err := s.Put("s", "cert", []byte("B")); !errors.Is(err, ErrSlotConflict) {
		t.Fatalf("a different value to a written slot should conflict, got %v", err)
	}
}

func TestUnknownSlotRefused(t *testing.T) {
	s := New(Options{})
	if err := s.Put("s", "not_a_slot", []byte("x")); !errors.Is(err, ErrUnknownSlot) {
		t.Fatalf("an unknown slot should be refused, got %v", err)
	}
}

func TestOversizeRefused(t *testing.T) {
	s := New(Options{MaxBytes: 8})
	if err := s.Put("s", "cert", make([]byte, 9)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("an oversize value should be refused, got %v", err)
	}
	if err := s.Put("s", "cert", make([]byte, 8)); err != nil {
		t.Fatalf("a value at the cap should be accepted: %v", err)
	}
}

func TestMalformedSessionRefused(t *testing.T) {
	s := New(Options{})
	for _, bad := range []string{"", "has/slash", "space here", "toolong" + string(make([]byte, 200))} {
		if err := s.Put(bad, "cert", []byte("x")); !errors.Is(err, ErrBadSession) {
			t.Fatalf("session %q should be refused, got %v", bad, err)
		}
	}
}

func TestSessionExpiryReapsValues(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	s := New(Options{Clock: clk, TTL: time.Minute})
	if err := s.Put("s", "cert", []byte("A")); err != nil {
		t.Fatal(err)
	}
	if s.SessionCount() != 1 {
		t.Fatalf("expected 1 live session, got %d", s.SessionCount())
	}
	clk.advance(2 * time.Minute)
	if _, err := s.Get("s", "cert"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired session should read as absent, got %v", err)
	}
	if s.SessionCount() != 0 {
		t.Fatalf("the expired session should have been reaped, got %d", s.SessionCount())
	}
}

func TestMaxSessionsCap(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	s := New(Options{Clock: clk, MaxSessions: 2, TTL: time.Hour})
	if err := s.Put("a", "cert", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("b", "cert", []byte("2")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("c", "cert", []byte("3")); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("a third session past the cap should be refused, got %v", err)
	}
	// Once the first two expire, room frees up.
	clk.advance(2 * time.Hour)
	if err := s.Put("c", "cert", []byte("3")); err != nil {
		t.Fatalf("after expiry a new session should fit: %v", err)
	}
}
