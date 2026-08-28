package indexers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Newznab is the usenet counterpart of Torznab, and the same wire protocol, so
// the ONE client parses it (ADR-0028: implement the protocol, not the product).
// These tests prove it does — on the two things that actually differ from a
// torrent feed, because everything else is byte-identical and already covered:
//
//   - the attribute elements carry the `newznab:` namespace prefix rather than
//     `torznab:`, and the parser must still read them (it matches on local name,
//     so pinning either namespace would break this);
//   - the release's source is an .nzb enclosure, not a magnet, and sourceOf must
//     fall through to it — a usenet release has no magnet at all.

const newznabCaps = `<?xml version="1.0" encoding="UTF-8"?>
<caps>
  <server title="Newznab" version="0.2.3"/>
  <limits default="100" max="100"/>
  <searching>
    <search available="yes"/>
    <tv-search available="yes"/>
    <movie-search available="yes"/>
  </searching>
</caps>`

// A real-shaped Newznab search feed: the newznab namespace on the root, an .nzb
// enclosure, and content attributes in newznab:attr elements. The apikey in the
// enclosure URL is an obviously-fake placeholder, not a credential.
const newznabFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/">
  <channel>
    <item>
      <title>Beacon Hill 2016 1080p WEB-DL x264</title>
      <guid>beacon-hill-1080</guid>
      <enclosure url="http://usenet-indexer.invalid/getnzb/beacon-hill-1080.nzb?apikey=NOT-A-REAL-KEY" length="1500000000" type="application/x-nzb"/>
      <newznab:attr name="resolution" value="1080"/>
      <newznab:attr name="video_codec" value="x264"/>
      <newznab:attr name="usenetdate" value="Wed, 21 Aug 2026 12:00:00 +0000"/>
    </item>
  </channel>
</rss>`

// newznabServer answers the caps handshake first and a search feed after, the
// way a real Newznab indexer does.
func newznabServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("t") == "caps" {
			_, _ = w.Write([]byte(newznabCaps))
			return
		}
		_, _ = w.Write([]byte(newznabFeed))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestANewznabFeedBecomesUsenetCandidates(t *testing.T) {
	srv := newznabServer(t)
	got, err := clientFor(t, srv.URL).Search(t.Context(),
		providers.Query{Title: "Beacon Hill", ContentType: "movie"})
	if err != nil {
		t.Fatalf("searching a newznab feed failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	c := got[0]

	if c.Title != "Beacon Hill 2016 1080p WEB-DL x264" {
		t.Errorf("title = %q", c.Title)
	}

	// The usenet source: an .nzb, reached because sourceOf falls through to the
	// enclosure when there is no magnet — the whole shape of a newznab release.
	src := c.Source.Reveal()
	if !strings.Contains(src, ".nzb") {
		t.Errorf("source %q is not the .nzb enclosure", src)
	}
	if strings.HasPrefix(src, "magnet:") {
		t.Errorf("a usenet release must not carry a magnet source: %q", src)
	}

	// A content attribute carried under the newznab: prefix is read — the proof
	// the parser matches on local name rather than a pinned namespace.
	if v, ok := c.Attributes[policy.AttrResolution]; !ok || v.Num != 1080 {
		t.Errorf("resolution from a newznab:attr was not parsed: got %v (present=%v)", v, ok)
	}
}

// A newznab-kind provider is served by this package's client — the Constructor
// accepts the kind, so an operator's `type: newznab` resolves to a working
// indexer rather than the registry's "configured, not implemented".
func TestTheConstructorServesTheNewznabKind(t *testing.T) {
	endpoint, err := url.Parse("http://usenet-indexer.invalid/api")
	if err != nil {
		t.Fatal(err)
	}
	p, handled, err := Constructor(providers.Resolved{
		Name:         "a-usenet-indexer",
		Kind:         providers.KindNewznab,
		Endpoint:     endpoint,
		Credential:   providers.TokenCredential("NOT-A-REAL-KEY"),
		Capabilities: []providers.Capability{providers.CapabilityIndexer},
	}, func() time.Time { return time.Now().UTC() })
	if err != nil {
		t.Fatalf("constructing a newznab provider failed: %v", err)
	}
	if !handled {
		t.Fatal("the indexer constructor did not claim the newznab kind")
	}
	if _, ok := p.(*Client); !ok {
		t.Fatalf("a newznab kind produced a %T, not the indexer client", p)
	}
}
