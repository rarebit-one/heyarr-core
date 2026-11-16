package providers

import (
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
)

// theSource stands in for a magnet. It contains something that would be
// unmistakable if it ever reached an error string or a log line, because that
// is the failure this value's type exists to prevent.
const theSource = "magnet:?xt=urn:btih:abc123&passkey=DO-NOT-LEAK-4f2a"

// A grab goes to ONE client, not to all of them.
//
// The distinction matters more than it looks: Search deliberately fans out
// across every indexer, and the obvious way to write Grab is to copy that loop.
// Doing so would hand the same magnet to every configured client and produce
// three copies of the same bytes in three queues, racing to be ingested.
func TestGrabRoutesToOneClientAndStops(t *testing.T) {
	first := NewFake("client-a", CapabilityDownload)
	second := NewFake("client-b", CapabilityDownload)
	third := NewFake("client-c", CapabilityDownload)

	reg := New(nil)
	for _, f := range []*Fake{first, second, third} {
		if err := reg.Register(f); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := reg.Grab(t.Context(), secret.Value(theSource), acquisition.ProtocolUnknown); err != nil {
		t.Fatal(err)
	}

	// Every client is checked, including the ones that must NOT have been
	// asked. Asserting only that the winner received it would pass for a
	// fan-out, which is the entire defect this guards.
	for _, tc := range []struct {
		name string
		fake *Fake
		want int
	}{
		{"the first client took it", first, 1},
		{"the second was not asked", second, 0},
		{"the third was not asked", third, 0},
	} {
		if got := len(tc.fake.Added()); got != tc.want {
			t.Errorf("%s: Add called %d times, want %d", tc.name, got, tc.want)
		}
	}
}

// A client that refuses does not end the grab — the next one is asked.
//
// The client that succeeds is deliberately the SECOND of three, and the third
// is healthy too. With the working client first, "walked past a refusal" and
// "asked the first client" are the same sequence and the test cannot tell them
// apart — the position-zero fixture mistake that has now been found twice in
// this repository in unrelated packages.
//
// The third being healthy is the other half: it proves the walk STOPS at the
// first success rather than continuing through everything that would accept.
func TestGrabWalksPastARefusalAndStopsAtTheFirstSuccess(t *testing.T) {
	refuses := NewFake("client-a", CapabilityDownload).
		FailWith(errors.New("the client is restarting"))
	accepts := NewFake("client-b", CapabilityDownload)
	alsoHealthy := NewFake("client-c", CapabilityDownload)

	reg := New(nil)
	for _, f := range []*Fake{refuses, accepts, alsoHealthy} {
		if err := reg.Register(f); err != nil {
			t.Fatal(err)
		}
	}

	transfer, provider, err := reg.Grab(t.Context(), secret.Value(theSource), acquisition.ProtocolUnknown)
	if err != nil {
		t.Fatal(err)
	}

	if provider != "client-b" {
		t.Errorf("the grab reports %q as the client, want client-b", provider)
	}
	if transfer.ID == "" {
		t.Error("the grab returned no transfer, so nothing downstream can find it")
	}
	if got := len(refuses.Added()); got != 0 {
		t.Errorf("the refusing client recorded %d adds, want 0", got)
	}
	if got := len(accepts.Added()); got != 1 {
		t.Errorf("the accepting client recorded %d adds, want 1", got)
	}
	if got := len(alsoHealthy.Added()); got != 0 {
		t.Errorf("the walk continued past a success: client-c recorded %d adds", got)
	}
}

// What the client is handed is the source, unchanged.
//
// Asserted separately from routing because a grab that reaches a client with an
// EMPTY source would pass every routing assertion above — the transfer comes
// back, the provider is named, the counts are right — and download nothing.
func TestGrabHandsTheClientTheSourceItWasGiven(t *testing.T) {
	client := NewFake("client-a", CapabilityDownload)
	reg := New(nil)
	if err := reg.Register(client); err != nil {
		t.Fatal(err)
	}

	if _, _, err := reg.Grab(t.Context(), secret.Value(theSource), acquisition.ProtocolUnknown); err != nil {
		t.Fatal(err)
	}

	added := client.Added()
	if len(added) != 1 {
		t.Fatalf("the client recorded %d adds, want 1", len(added))
	}
	if got := added[0].Reveal(); got != theSource {
		t.Errorf("the client was handed %q, want the source it was given", got)
	}
}

// An empty source is refused before any client is asked.
//
// It is ErrNoSource specifically, and not a generic failure, because the
// handler branches on it: a candidate the indexer offered no way to fetch
// cannot be retried into working, so the want is failed rather than left in
// SELECTED — which is exactly the resting state #225 is about.
func TestGrabRefusesAnEmptySourceWithoutAskingAnybody(t *testing.T) {
	client := NewFake("client-a", CapabilityDownload)
	reg := New(nil)
	if err := reg.Register(client); err != nil {
		t.Fatal(err)
	}

	_, _, err := reg.Grab(t.Context(), "", acquisition.ProtocolUnknown)
	if !errors.Is(err, ErrNoSource) {
		t.Fatalf("grab with no source returned %v, want ErrNoSource", err)
	}
	if got := len(client.Added()); got != 0 {
		t.Errorf("a client was asked %d times for a candidate with no source", got)
	}
}

// With no download client at all the answer is ErrNoProvider, not ErrNoSource.
//
// The two are separate errors because they lead to opposite responses: a node
// with no client will not acquire anything by retrying, and a candidate with no
// source will not become fetchable however healthy the clients are. Collapsing
// them makes "why is nothing downloading" unanswerable.
func TestGrabWithNoDownloadClientIsNotTheSameAsNoSource(t *testing.T) {
	reg := New(nil)
	if err := reg.Register(NewFake("an-indexer", CapabilityIndexer)); err != nil {
		t.Fatal(err)
	}

	_, _, err := reg.Grab(t.Context(), secret.Value(theSource), acquisition.ProtocolUnknown)
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("grab with no download client returned %v, want ErrNoProvider", err)
	}
	if errors.Is(err, ErrNoSource) {
		t.Error("a missing client was reported as a missing source")
	}
}

// When every client refuses, the error names them all — and none of them
// quotes the magnet.
//
// The redaction is asserted on the ERROR rather than on the type, because the
// type's own redaction is tested where it lives. What is under test here is
// this package's discipline about where it puts the value: Grab builds its
// refusal string with %v, which routes through the redaction, and %w or %s on
// the revealed string would not.
func TestGrabReportsEveryRefusalWithoutQuotingTheSource(t *testing.T) {
	reg := New(nil)
	for _, name := range []string{"client-a", "client-b"} {
		f := NewFake(name, CapabilityDownload).
			FailWith(errors.New("the client is restarting"))
		if err := reg.Register(f); err != nil {
			t.Fatal(err)
		}
	}

	_, _, err := reg.Grab(t.Context(), secret.Value(theSource), acquisition.ProtocolUnknown)
	if err == nil {
		t.Fatal("every client refused and the grab reported success")
	}
	for _, name := range []string{"client-a", "client-b"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}
	if strings.Contains(err.Error(), "DO-NOT-LEAK") {
		t.Fatalf("the source reached an error string: %v", err)
	}
}

// protocolFake is a download client that says which protocol it fetches, as
// every real client does (internal/downloads/protocol.go).
type protocolFake struct {
	*Fake
	protocol acquisition.Protocol
}

func (p protocolFake) Protocol() acquisition.Protocol { return p.protocol }

// A grab routes by the release's protocol, not by which client accepts first
// (#836). A torrent client and a direct-download client both accept an http(s)
// URL, so configuration order decided whether a direct link became a failed
// torrent add or a .torrent was ingested as if it were the payload. Each order
// is tried, because a fix that only held in one would be the bug again.
func TestGrabRoutesByTheReleasesProtocolInEitherOrder(t *testing.T) {
	const link = "https://downloads.example/files/release.bin"
	for _, order := range []string{"torrent first", "direct first"} {
		for _, tc := range []struct {
			protocol acquisition.Protocol
			want     string
		}{
			{acquisition.ProtocolTorrent, "transmission"},
			{acquisition.ProtocolDirect, "http"},
		} {
			t.Run(order+"/"+string(tc.protocol), func(t *testing.T) {
				torrent := protocolFake{NewFake("transmission", CapabilityDownload), acquisition.ProtocolTorrent}
				direct := protocolFake{NewFake("http", CapabilityDownload), acquisition.ProtocolDirect}
				clients := []protocolFake{torrent, direct}
				if order == "direct first" {
					clients = []protocolFake{direct, torrent}
				}
				reg := New(nil)
				for _, c := range clients {
					if err := reg.Register(c); err != nil {
						t.Fatal(err)
					}
				}

				_, provider, err := reg.Grab(t.Context(), secret.Value(link), tc.protocol)
				if err != nil {
					t.Fatal(err)
				}
				if provider != tc.want {
					t.Fatalf("a %s release went to %s, want %s", tc.protocol, provider, tc.want)
				}
				for _, c := range clients {
					want := 0
					if c.Name() == tc.want {
						want = 1
					}
					if got := len(c.Added()); got != want {
						t.Errorf("%s: Add called %d times, want %d", c.Name(), got, want)
					}
				}
			})
		}
	}
}

// A release whose provider named no protocol is offered to every client, as
// before (#836): rows written before the protocol existed must still grab.
// And a protocol no configured client fetches is refused as the node not
// being equipped, rather than handed to a client of another protocol.
func TestGrabWithoutAProtocolOrWithoutAClientForIt(t *testing.T) {
	torrent := protocolFake{NewFake("transmission", CapabilityDownload), acquisition.ProtocolTorrent}
	reg := New(nil)
	if err := reg.Register(torrent); err != nil {
		t.Fatal(err)
	}
	if _, provider, err := reg.Grab(t.Context(), secret.Value(theSource), acquisition.ProtocolUnknown); err != nil || provider != "transmission" {
		t.Fatalf("an unrouted release: provider=%q err=%v, want transmission", provider, err)
	}
	_, _, err := reg.Grab(t.Context(), secret.Value(theSource), acquisition.ProtocolUsenet)
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("a usenet release with only a torrent client: err=%v, want ErrNoProvider", err)
	}
	if got := len(torrent.Added()); got != 1 {
		t.Fatalf("the torrent client was handed %d releases, want only the unrouted one", got)
	}
	if strings.Contains(err.Error(), "DO-NOT-LEAK") {
		t.Fatalf("the source reached the error: %v", err)
	}
}
