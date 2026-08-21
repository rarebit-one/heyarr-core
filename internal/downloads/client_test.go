package downloads

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers/fixtures"
)

// The Transmission client, driven against the REAL captured corpus.
//
// fixtures.Corpus.Server() speaks HTTP, so these exercise the real transport —
// the session handshake, the envelope, the field decoding — rather than a stub
// of it. A harness that handed parsed values to a test would prove the harness
// works; this proves the client parses what a real Transmission actually sent.

const corpusRoot = "../providers/fixtures/testdata"

func corpus(t *testing.T) fixtures.Corpus {
	t.Helper()
	c, err := fixtures.Load(corpusRoot, "transmission")
	if errors.Is(err, fixtures.ErrNoCorpus) {
		t.Skip("no Transmission corpus committed")
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var fixedNow = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

// capturedLabel is the label the corpus's transfers actually carry.
//
// The capture tagged its torrents `heyarr-fixture-capture` rather than the
// production default, and that turns out to be the more useful corpus: it means
// the same fixtures serve BOTH sides of the safety property. A client
// configured with this label sees them; one with any other label sees nothing,
// which is exactly what "a transfer that is not ours is invisible" has to mean.
const capturedLabel = "heyarr-fixture-capture"

func clientFor(t *testing.T, url string, maps ...Mapping) *Client {
	t.Helper()
	return labelledClient(t, url, capturedLabel, maps...)
}

func labelledClient(t *testing.T, url, label string, maps ...Mapping) *Client {
	t.Helper()
	pm, err := ParsePathMap("test", maps)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{
		Name: "test", Endpoint: url, PathMap: pm, Label: label,
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The corpus replays a 409 for the handshake and 200s for everything else, but
// its matching is on method+path and every Transmission call is
// POST /transmission/rpc — so a live-ish server is needed that behaves the way
// the daemon does: 409 until the id is replayed, then the recorded body.
//
// This is NOT a stand-in for the corpus. The BODIES are the captured ones,
// verbatim; what this adds is the session state machine, which is behaviour
// rather than payload and cannot be recorded as a single exchange.
func replayServer(t *testing.T, c fixtures.Corpus) *httptest.Server {
	t.Helper()

	handshake, ok := c.Find("session-handshake-409")
	if !ok {
		t.Fatal("the corpus has no session-handshake-409")
	}
	sessionID := handshake.Response.Headers["X-Transmission-Session-Id"]
	if sessionID == "" {
		t.Fatal("the recorded handshake carries no session id")
	}

	bodyFor := func(name string) string {
		e, ok := c.Find(name)
		if !ok {
			t.Fatalf("the corpus has no %s", name)
		}
		return e.Response.Body
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Transmission-Session-Id") != sessionID {
			w.Header().Set("X-Transmission-Session-Id", sessionID)
			w.WriteHeader(http.StatusConflict)
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		_ = decodeJSONBody(r, &req)

		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "session-get":
			_, _ = w.Write([]byte(bodyFor("session-get")))
		case "torrent-get":
			_, _ = w.Write([]byte(bodyFor("torrent-get")))
		default:
			_, _ = w.Write([]byte(`{"result":"success","arguments":{}}`))
		}
	}))
}

// THE test for the handshake. A client that treats 409 as an error works
// against every hand-written fixture and fails against every real instance.
func TestTheSessionHandshakeIsNotAnError(t *testing.T) {
	c := corpus(t)
	srv := replayServer(t, c)
	defer srv.Close()

	client := clientFor(t, srv.URL)
	health := client.Check(context.Background())

	if !health.Healthy {
		t.Fatalf("the 409 handshake was treated as a failure: %s", health.Detail)
	}
	if health.Version == "" {
		t.Error("a healthy check reports the version the service gave")
	}
	// And the id was actually kept, so a second call does not re-handshake
	// from scratch. Asserting the state rather than the outcome, because the
	// outcome is identical either way and only one of them is correct.
	if client.rpc.session() == "" {
		t.Error("the session id was not retained, so every call would re-handshake")
	}
}

// The facts the capture settled. These are assertions about a REAL instance,
// which is what makes them worth having.
func TestTheCapturedInstanceReportsWhatWeBuiltAgainst(t *testing.T) {
	c := corpus(t)
	srv := replayServer(t, c)
	defer srv.Close()

	client := clientFor(t, srv.URL)
	if h := client.Check(context.Background()); !h.Healthy {
		t.Fatalf("check failed: %s", h.Detail)
	}
	s := client.Session()

	if s.RPCVersion < labelsRPCVersion {
		t.Fatalf("rpc-version %d — the corpus was captured at 19, above the %d that "+
			"introduced labels, and the primary path assumes it", s.RPCVersion, labelsRPCVersion)
	}
	if !s.SupportsLabels() {
		t.Error("labels are the primary path on this instance")
	}
	// The gotcha. Mid-transfer paths lie on this instance, which is why
	// resolvePath is only reached on completion.
	if !s.IncompleteDirEnabled {
		t.Error("the captured instance has incomplete-dir enabled; a client that " +
			"resolved downloadDir + name mid-transfer would produce a path that " +
			"does not exist")
	}
	if s.IncompleteDir == "" || s.DownloadDir == "" {
		t.Errorf("session-get must carry both directories, got %q and %q",
			s.DownloadDir, s.IncompleteDir)
	}
}

// 🔴 The finding this client is built around: a tracker failure is invisible at
// the top level, so a client watching errorString sees a healthy transfer
// forever.
func TestATrackerFailureSurfacesEvenThoughErrorStringIsEmpty(t *testing.T) {
	c := corpus(t)
	srv := replayServer(t, c)
	defer srv.Close()

	client := clientFor(t, srv.URL)
	if h := client.Check(context.Background()); !h.Healthy {
		t.Fatalf("check failed: %s", h.Detail)
	}

	transfers, err := client.Transfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(transfers) < 2 {
		t.Fatalf("the corpus holds %d of our transfers; it needs a healthy one and a "+
			"stalled one", len(transfers))
	}

	var stalled, healthy int
	for _, tr := range transfers {
		if strings.Contains(tr.Error, string(TroubleTrackerUnreachable)) {
			stalled++
		}
		if tr.Error == "" {
			healthy++
		}
	}
	if stalled == 0 {
		t.Error("no transfer reported a tracker failure — the corpus contains one " +
			"whose errorString is EMPTY while its trackerStats say the tracker is " +
			"unreachable, and missing it is the bug this client exists to avoid")
	}
	if healthy == 0 {
		t.Error("every transfer reported trouble, so the detection is not discriminating")
	}
}

// Paths are resolved only on completion, because with incomplete-dir enabled a
// mid-transfer path does not exist.
func TestAPathIsOnlyResolvedOnCompletion(t *testing.T) {
	c := corpus(t)
	srv := replayServer(t, c)
	defer srv.Close()

	client := clientFor(t, srv.URL)
	if h := client.Check(context.Background()); !h.Healthy {
		t.Fatal(h.Detail)
	}
	transfers, err := client.Transfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, tr := range transfers {
		switch {
		case tr.Done && tr.Path == "":
			t.Errorf("%s is complete and has no path, so ingest has nowhere to look", tr.Name)
		case !tr.Done && tr.Path != "":
			t.Errorf("%s is mid-transfer and reports %q — with incomplete-dir enabled "+
				"that path does not exist yet", tr.Name, tr.Path)
		}
	}
}

// Identity is the infohash, never the name.
func TestTransfersAreIdentifiedByInfohash(t *testing.T) {
	c := corpus(t)
	srv := replayServer(t, c)
	defer srv.Close()

	client := clientFor(t, srv.URL)
	transfers, err := client.Transfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range transfers {
		if len(tr.ID) != 40 {
			t.Errorf("%s has id %q, which is not an infohash — names get renamed, "+
				"collide and do not survive a restart", tr.Name, tr.ID)
		}
		if tr.ID == tr.Name {
			t.Errorf("%s is identified by its name", tr.Name)
		}
	}
}

func decodeJSONBody(r *http.Request, out any) error {
	defer func() { _ = r.Body.Close() }()
	return decodeJSON(r, out)
}

// 🔴 THE SAFETY PROPERTY. A transfer Heyarr did not queue is INVISIBLE — not
// merely excluded from mutation, but absent from the list entirely, so that no
// caller can act on one it was never shown.
//
// The same corpus drives both halves. Its transfers carry one label; a client
// configured with any other sees nothing at all, which is what an operator's
// own torrents look like to Heyarr.
//
// An acquisition system that can delete an operator's data because a name
// matched is one nobody should run.
func TestATransferThatIsNotOursIsInvisible(t *testing.T) {
	c := corpus(t)
	srv := replayServer(t, c)
	defer srv.Close()

	// A client with the production default, against a queue full of transfers
	// labelled for something else — which is precisely an operator's own
	// torrents sitting alongside Heyarr's.
	foreign := labelledClient(t, srv.URL, DefaultLabel)
	transfers, err := foreign.Transfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(transfers) != 0 {
		t.Fatalf("a client saw %d transfer(s) it did not queue; they must be invisible",
			len(transfers))
	}

	// The control: the same corpus, the label it was captured under. Without
	// this, the assertion above would pass against a client that saw nothing
	// because everything was broken.
	ours := clientFor(t, srv.URL)
	mine, err := ours.Transfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) == 0 {
		t.Fatal("the matching label saw nothing either, so the assertion above " +
			"proves only that the client is broken")
	}
}

// Removal refuses a transfer that is not ours, by id.
//
// Enforced in the client rather than trusted to callers: a stale row, a bug or
// a copied infohash must not be able to reach an operator's data.
func TestRemoveRefusesAForeignTransfer(t *testing.T) {
	cp := corpus(t)
	srv := replayServer(t, cp)
	defer srv.Close()

	ours := clientFor(t, srv.URL)
	mine, err := ours.Transfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) == 0 {
		t.Fatal("setup: the corpus has no transfers under the captured label")
	}

	// A client that did not queue these tries to remove one of them by its
	// real, correct infohash. It must still refuse.
	foreign := labelledClient(t, srv.URL, DefaultLabel)
	err = foreign.Remove(context.Background(), mine[0].ID, true)
	if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("removing a foreign transfer returned %v; it must refuse with ErrNotOurs", err)
	}

	// And the control: the owner can remove it, so the refusal above is about
	// ownership rather than about removal being broken.
	if err := ours.Remove(context.Background(), mine[0].ID, false); err != nil {
		t.Fatalf("the owner could not remove its own transfer: %v", err)
	}
}
