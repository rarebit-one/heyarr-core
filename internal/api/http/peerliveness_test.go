package httpapi_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Liveness on the request path (§31, M4-10).
//
// Health is OBSERVED rather than declared, and the strongest observation
// available is a peer talking to us: a request it opened, on a connection it
// dialled, as a side effect of work it was already doing. These tests pin that
// the guard makes that observation — and that it makes it only for requests it
// admitted.

type membershipFunc func(ctx context.Context, pub []byte) (bool, error)

func (f membershipFunc) IsMember(ctx context.Context, pub []byte) (bool, error) { return f(ctx, pub) }

type recordingLiveness struct {
	mu   sync.Mutex
	keys [][]byte
	err  error
}

func (r *recordingLiveness) Seen(_ context.Context, pub []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, append([]byte(nil), pub...))
	return r.err
}

func (r *recordingLiveness) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.keys)
}

func newLivenessServer(t *testing.T, member bool, liveness httpapi.PeerLiveness, key []byte) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.HTTP.Auth.Enabled = false
	cfg.HTTP.UnixSocket = ""

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, DB: db, Events: eventLog, Logger: slog.New(slog.DiscardHandler),
		KnownSchemaVersion: 20, SchemaVersion: 20,
		PeerMembership: membershipFunc(func(context.Context, []byte) (bool, error) {
			return member, nil
		}),
		PeerLiveness: liveness,
		PresentedPeerKey: func(*http.Request) ([]byte, bool) {
			if key == nil {
				return nil, false
			}
			return key, true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// getSystem issues an unauthenticated GET /api/v1/system against ts.
//
// Named for the endpoint rather than the verb because this package already
// has a `get` helper (peercredential_test.go) with a different signature; two
// helpers called `get` in one test binary is a collision waiting for whichever
// file is written next.
func getSystem(t *testing.T, ts *httptest.Server) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/system", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// TestAPeerRequestRecordsLiveness: an admitted peer request is an observation.
func TestAPeerRequestRecordsLiveness(t *testing.T) {
	t.Parallel()
	key := []byte("a presented peer key")
	rec := &recordingLiveness{}
	ts := newLivenessServer(t, true, rec, key)

	if code := getSystem(t, ts); code != http.StatusOK {
		t.Fatalf("GET /api/v1/system = %d, want 200", code)
	}
	if rec.count() != 1 {
		t.Fatalf("%d liveness records for one admitted peer request, want 1", rec.count())
	}
	if string(rec.keys[0]) != string(key) {
		t.Errorf("recorded key = %q, want the key the connection presented", rec.keys[0])
	}
}

// TestANonMemberRequestRecordsNoLiveness. The recording happens AFTER the
// membership guard admits the request, and that ordering is the assertion: a
// key nobody pinned is not a peer whose liveness this system tracks, and
// recording it would let an unenrolled machine write to the peers table by
// knocking.
func TestANonMemberRequestRecordsNoLiveness(t *testing.T) {
	t.Parallel()
	rec := &recordingLiveness{}
	ts := newLivenessServer(t, false, rec, []byte("a key nobody pinned"))

	if code := getSystem(t, ts); code != http.StatusForbidden {
		t.Fatalf("GET /api/v1/system from a non-member = %d, want 403", code)
	}
	if rec.count() != 0 {
		t.Errorf("%d liveness records for a refused request, want 0", rec.count())
	}
}

// TestANonPeerRequestRecordsNoLiveness. An ordinary client with a bearer token
// is not a peer, and there is nothing about its liveness to record.
func TestANonPeerRequestRecordsNoLiveness(t *testing.T) {
	t.Parallel()
	rec := &recordingLiveness{}
	ts := newLivenessServer(t, true, rec, nil)

	if code := getSystem(t, ts); code != http.StatusOK {
		t.Fatalf("GET /api/v1/system = %d, want 200", code)
	}
	if rec.count() != 0 {
		t.Errorf("%d liveness records for a non-peer request, want 0", rec.count())
	}
}

// TestLivenessFailureDoesNotFailTheRequest. Recording that somebody is alive
// must never be able to break what they asked for: the request is the peer's
// work, and the observation is Heyarr's bookkeeping about it.
func TestLivenessFailureDoesNotFailTheRequest(t *testing.T) {
	t.Parallel()
	rec := &recordingLiveness{err: errors.New("the database is having a moment")}
	ts := newLivenessServer(t, true, rec, []byte("a presented peer key"))

	if code := getSystem(t, ts); code != http.StatusOK {
		t.Errorf("GET /api/v1/system = %d, want 200 — a liveness write failing is not the peer's problem", code)
	}
}
