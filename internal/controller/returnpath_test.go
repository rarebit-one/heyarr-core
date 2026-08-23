package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/reachability"
)

// The return-path probe behind GET /peer/v1/reachback (#186, ADR-0037).
//
// The property that matters most here is the one a credentialled probe would
// break: this must answer "reachable" for a peer that has NOT enrolled this
// node yet. Enrolment is two operators running two commands, and between them
// the far end refuses this node's certificate — a probe that needed the
// handshake to succeed would refuse the enrolment that was about to make it
// succeed, and the documented order could never be completed. So the peers
// registered below pin keys that no listener presents, and the probe is still
// expected to say reachable.

func registerPeerAt(t *testing.T, store *membership.Store, name, endpoint string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	res, err := store.Register(context.Background(), membership.Registration{
		Name: name, Site: "site-b", Endpoint: endpoint, PublicKey: pub,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.Member.PeerID
}

func TestProbeReturnPath(t *testing.T) {
	// A listener that accepts connections and speaks nothing. It stands in for
	// a peer surface, which answers no plaintext request either.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	store, _ := realFabric(t)
	up := registerPeerAt(t, store, "peer-up", "https://"+listener.Addr().String())
	// Port 9 is discard: reserved, and refusing connections everywhere.
	down := registerPeerAt(t, store, "peer-down", "https://127.0.0.1:9")
	unaddressed := registerPeerAt(t, store, "peer-unaddressed", "")
	local := registerPeerAt(t, store, "peer-local", "unix:///tmp/heyarr-peer.sock")

	prober := returnPathProber{store: store}
	cases := []struct {
		name   string
		peerID string
		want   reachability.Result
	}{
		{"a peer that accepts connections", up, reachability.ResultReachable},
		{"a peer whose port refuses them", down, reachability.ResultUnreachable},
		{"a peer with no endpoint at all", unaddressed, reachability.ResultUnknown},
		{"a peer on this host", local, reachability.ResultUnknown},
		{"a peer this node does not know", "01990000-0000-7000-8000-00000000none", reachability.ResultUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := prober.ProbeReturnPath(t.Context(), tc.peerID)
			if err != nil {
				t.Fatalf("probing: %v", err)
			}
			// assert_eq on the enum. `unreachable` and `unknown` call for
			// opposite decisions at the far end — one refuses an enrolment
			// and the other must never — so a substring check here would be
			// checking the one thing that must not be approximate.
			if got != tc.want {
				t.Fatalf("result = %q, want %q", got, tc.want)
			}
		})
	}
}
