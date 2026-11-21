package peerapi_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// recordingFanOut is a FanOutSource that records what it was asked.
type recordingFanOut struct {
	mu     sync.Mutex
	signer string
	op     string
	limit  int
}

func (r *recordingFanOut) FanOutFrom(_ context.Context, signer, op string, limit int) (peerapi.FanOutAnswer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.signer, r.op, r.limit = signer, op, limit
	return peerapi.FanOutAnswer{Site: "site-a", Applications: []peerapi.FanOutApplication{
		{OpHash: op, Outcome: "applied", AppliedAt: "2026-11-21T08:00:00Z", State: "downloading"},
	}}, nil
}

func fanOutURL(l *listener, op string) string {
	u := "https://" + l.addr + peerapi.FanOutPath
	if op != "" {
		u += "?op=" + op
	}
	return u
}

// TestFanOutAnswersForTheCertificatesKey (#924): the sibling answers for the
// ops the asking CERTIFICATE's key signed — the signer is never a parameter —
// narrowed to the op asked about, bounded.
func TestFanOutAnswersForTheCertificatesKey(t *testing.T) {
	t.Parallel()
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	src := &recordingFanOut{}
	l := serveFanOut(t, a, root, src)

	status, body, _, err := peerGet(t, dialler(t, b, root), fanOutURL(l, "blake3:abc")+"&signer=someone-else")
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET fan-out: status=%d err=%v\n%s", status, err, body)
	}
	var got peerapi.FanOutAnswer
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("fan-out body is not decodable: %v\n%s", err, body)
	}
	if got.Site != "site-a" || len(got.Applications) != 1 || got.Applications[0].Outcome != "applied" ||
		got.Applications[0].State != "downloading" {
		t.Errorf("answer = %+v", got)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.signer != identity.FormatPublicKey(b.pub) || src.op != "blake3:abc" ||
		src.limit != peerapi.MaxFanOutApplications {
		t.Errorf("asked the source for signer %q, op %q, limit %d; want the caller's key, the op and the bound",
			src.signer, src.op, src.limit)
	}
}

// TestFanOutRefusals: no source is 503, an `op` that is not an op hash is 400.
func TestFanOutRefusals(t *testing.T) {
	t.Parallel()
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())

	status, _, _, err := peerGet(t, dialler(t, b, root), fanOutURL(serve(t, a, root), ""))
	if err != nil || status != http.StatusServiceUnavailable {
		t.Errorf("no source: status=%d err=%v, want 503", status, err)
	}
	l := serveFanOut(t, a, root, &recordingFanOut{})
	status, _, _, err = peerGet(t, dialler(t, b, root), fanOutURL(l, strings.Repeat("x", 200)))
	if err != nil || status != http.StatusBadRequest {
		t.Errorf("an overlong op: status=%d err=%v, want 400", status, err)
	}
}

func serveFanOut(t *testing.T, self *peerNode, members mtls.Membership, src peerapi.FanOutSource) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		FanOut:     src,
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
		_ = srv.Shutdown(ctx)
	})
	return &listener{srv: srv, self: self, addr: srv.Addr(), logs: logs}
}
