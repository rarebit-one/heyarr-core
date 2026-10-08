package replication_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/replication"
)

// epochState is a peerapi.StateStore that holds only what the key-epoch wire
// needs: a current epoch per space and the history rows that set it. It answers
// with the peer surface's own sentinels, as the controller's adapter does.
type epochState struct {
	mu      sync.Mutex
	epoch   int
	history map[int]string
	wraps   map[string]int // recipient -> epoch
}

func (s *epochState) HeadsFor(context.Context, string) ([]string, error) { return nil, nil }
func (s *epochState) ChangesFor(context.Context, string) ([]protocol.EncryptedChange, error) {
	return nil, nil
}
func (s *epochState) PutChange(context.Context, protocol.EncryptedChange) error { return nil }
func (s *epochState) PutSpace(context.Context, string, string) error            { return nil }
func (s *epochState) LatestSnapshotFor(context.Context, string) (protocol.EncryptedSnapshot, bool, error) {
	return protocol.EncryptedSnapshot{}, false, nil
}
func (s *epochState) PutSnapshot(context.Context, protocol.EncryptedSnapshot) error { return nil }

func (s *epochState) PutWrappedKey(_ context.Context, _, recipient string, _ []byte, epoch int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case epoch < s.epoch:
		return peerapi.ErrKeySuperseded
	case epoch > s.epoch:
		return peerapi.ErrKeyEpochAhead
	}
	s.wraps[recipient] = epoch
	return nil
}

func (s *epochState) PutKeyHistory(_ context.Context, _ string, epoch int, sealedPrev []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if held, ok := s.history[epoch]; ok && held != string(sealedPrev) {
		return peerapi.ErrKeyHistoryFork
	}
	s.history[epoch] = string(sealedPrev)
	if epoch > s.epoch {
		s.epoch = epoch
	}
	return nil
}

func material(t *testing.T, peerID string) (*mtls.Material, mtls.Peer) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: priv, PeerID: peerID, Lifetime: time.Hour, RenewBefore: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return m, mtls.Peer{PeerID: peerID, Name: peerID, PublicKey: pub}
}

// TestClientKeyEpochWire drives the real mTLS client against the real peer
// surface: a history row and an epoch-tagged wrap land, and a wrap the target
// refuses as superseded comes back as ErrWrapSuperseded — the one refusal the
// reconcile skips — while one ahead of the target's history stays an error.
func TestClientKeyEpochWire(t *testing.T) {
	t.Parallel()
	const space = "0199a0a0-0000-7000-8000-0000000000cc"
	serverMat, serverPeer := material(t, "peer-a")
	clientMat, clientPeer := material(t, "peer-b")
	st := &epochState{history: map[int]string{}, wraps: map[string]int{}}
	srv, err := peerapi.New(peerapi.Options{
		Addr: "127.0.0.1:0", Material: serverMat, Members: mtls.PinnedKey(clientPeer),
		SelfPeerID: serverPeer.PeerID, State: st, Logger: slog.New(slog.DiscardHandler),
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

	ctx := context.Background()
	c := replication.NewClient(clientMat, nil)
	tgt := replication.Target{Peer: serverPeer, Endpoint: "https://" + srv.Addr()}
	const recip = "x25519:aa"

	if err := c.PushWrappedKey(ctx, tgt, space, recip, []byte("k0"), 0); err != nil {
		t.Fatalf("epoch-0 wrap: %v", err)
	}
	if err := c.PushWrappedKey(ctx, tgt, space, recip, []byte("k1"), 1); err == nil || errors.Is(err, replication.ErrWrapSuperseded) {
		t.Fatalf("a wrap ahead of the history = %v, want a plain error", err)
	}
	if err := c.PushKeyHistory(ctx, tgt, space, 1, []byte("k0-under-k1")); err != nil {
		t.Fatalf("history row: %v", err)
	}
	if err := c.PushKeyHistory(ctx, tgt, space, 1, []byte("k0-under-k1")); err != nil {
		t.Fatalf("idempotent history row: %v", err)
	}
	if err := c.PushKeyHistory(ctx, tgt, space, 1, []byte("fork")); err == nil {
		t.Fatal("a forked history row was accepted")
	}
	if err := c.PushWrappedKey(ctx, tgt, space, recip, []byte("k1"), 1); err != nil {
		t.Fatalf("epoch-1 wrap: %v", err)
	}
	if err := c.PushWrappedKey(ctx, tgt, space, recip, []byte("k0"), 0); !errors.Is(err, replication.ErrWrapSuperseded) {
		t.Fatalf("a superseded wrap = %v, want ErrWrapSuperseded", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.epoch != 1 || st.wraps[recip] != 1 {
		t.Fatalf("target ended at epoch %d with the wrap at %d, want 1 and 1", st.epoch, st.wraps[recip])
	}
}
