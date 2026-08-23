//nolint:bodyclose // responses are closed by peerGet
package peerapi_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/reachability"
)

// GET /peer/v1/reachback (#186, ADR-0037).
//
// The route answers a question about the network BETWEEN two nodes rather than
// about the caller, and that makes one property load-bearing above all others:
// the caller supplies NOTHING. The address probed is this node's own record of
// the caller, so a member cannot use the route to dial wherever it likes. The
// recording prober below is what asserts that — it records the peer id it was
// asked about, and the request carries no target for it to have been given.

type recordingProber struct {
	askedAbout []string
	result     reachability.Result
	target     string
}

func (p *recordingProber) ProbeReturnPath(
	_ context.Context, peerID string,
) (reachability.Result, string, error) {
	p.askedAbout = append(p.askedAbout, peerID)
	return p.result, p.target, nil
}

func serveWithReturnPath(t *testing.T, self *peerNode, members mtls.Membership, prober peerapi.ReturnPathProber) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		ReturnPath: prober,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutting the peer surface down: %v", err)
		}
	})
	return &listener{srv: srv, self: self, addr: srv.Addr(), logs: logs}
}

func (l *listener) reachbackURL() string {
	return "https://" + l.addr + peerapi.Prefix + "/reachback"
}

func decodeReachback(t *testing.T, body string) peerapi.Reachback {
	t.Helper()
	var out peerapi.Reachback
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the peer surface answered something that is not a reachback: %v\n%s", err, body)
	}
	return out
}

// TestReachbackReportsTheProbeAgainstTheCallersOwnRecord: the answer is the
// probe's, the peer asked about is the CERTIFICATE's peer, and the target
// comes back so a caller can see which address was tried.
func TestReachbackReportsTheProbeAgainstTheCallersOwnRecord(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	prober := &recordingProber{result: reachability.ResultReachable, target: "https://peer-b.invalid:8385"}
	l := serveWithReturnPath(t, a, root, prober)

	status, body, _, err := peerGet(t, dialler(t, b, root), l.reachbackURL())
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("reachback answered %d: %s", status, body)
	}
	got := decodeReachback(t, body)
	// assert_eq on the enum: `unreachable` contains neither more nor less
	// than itself, and a substring check here would accept `unknown` for
	// `known` shapes of the same word in a future value.
	if got.Result != reachability.ResultReachable {
		t.Errorf("result = %q, want %q", got.Result, reachability.ResultReachable)
	}
	if got.Target != "https://peer-b.invalid:8385" {
		t.Errorf("target = %q, want the address this node holds for the caller", got.Target)
	}
	if len(prober.askedAbout) != 1 || prober.askedAbout[0] != b.peerID {
		t.Errorf("the prober was asked about %v, want exactly [%s] — the acting peer comes from "+
			"the certificate and the request carries no target at all", prober.askedAbout, b.peerID)
	}
}

// TestReachbackReportsAnUnreachableReturnPath is #186's observed case, as the
// far end sees it.
func TestReachbackReportsAnUnreachableReturnPath(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serveWithReturnPath(t, a, root,
		&recordingProber{result: reachability.ResultUnreachable, target: "https://peer-b.invalid:8385"})

	_, body, _, err := peerGet(t, dialler(t, b, root), l.reachbackURL())
	if err != nil {
		t.Fatal(err)
	}
	got := decodeReachback(t, body)
	if got.Result != reachability.ResultUnreachable {
		t.Fatalf("result = %q, want %q", got.Result, reachability.ResultUnreachable)
	}
	if got.Detail == "" {
		t.Error("an unreachable return path came back with no explanation")
	}
}

// TestReachbackWithoutAProberIsUnknownRatherThanAnError: a node that cannot
// probe answers in the vocabulary, not with a 503. Unknown is what the
// caller's decision table needs to see, and a failure would be read as a
// fault in the network by every caller that treats an error as one.
func TestReachbackWithoutAProberIsUnknownRatherThanAnError(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serve(t, a, root)

	status, body, _, err := peerGet(t, dialler(t, b, root), l.reachbackURL())
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("reachback answered %d on a node with no prober: %s", status, body)
	}
	got := decodeReachback(t, body)
	if got.Result != reachability.ResultUnknown {
		t.Fatalf("result = %q, want %q", got.Result, reachability.ResultUnknown)
	}
	if got.Target != "" {
		t.Errorf("a node that probed nothing named %q as the target", got.Target)
	}
}
