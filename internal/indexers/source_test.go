package indexers

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Every candidate a real server offers must carry something fetchable.
//
// # Why this test exists at all
//
// It was written because a SABOTAGE PASSED. Making sourceOf find the magnet and
// discard it broke nothing: the package had thorough coverage of ids, titles
// and attributes, and none of it noticed that the one field a download client
// needs had stopped being extracted. That is exactly the shape of #225 one
// layer down — a value quietly not carried, with everything around it green.
//
// The two captured servers cover both real shapes, and they differ in the way
// that matters:
//
//	jackett   a magnet, in a magneturl attr and in the enclosure
//	prowlarr  an http download URL — WITH AN API KEY IN THE QUERY STRING
//
// The second is why Source is a secret.Value and not a string.
func TestEveryCandidateFromARealFeedCanBeFetched(t *testing.T) {
	for _, server := range []string{"jackett", "prowlarr"} {
		t.Run(server, func(t *testing.T) {
			got, err := searchAgainst(t, server, "search-with-results",
				providers.Query{Title: "ubuntu"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 {
				t.Fatal("a feed with items produced no candidates, so this asserts nothing")
			}
			for _, c := range got {
				source := c.Source.Reveal()
				if strings.TrimSpace(source) == "" {
					t.Errorf("%q has no source, so it can be explained and never fetched", c.Title)
					continue
				}
				// A fetchable scheme, not merely a non-empty string. A details
				// page URL is non-empty and useless to a download client.
				if !strings.HasPrefix(source, "magnet:") && !strings.HasPrefix(source, "http") {
					t.Errorf("%q has source %q, which no download client can act on",
						c.Title, source)
				}
			}
		})
	}
}

// The credential in a source must not reach the candidate's id.
//
// prowlarr's enclosure URL carries `apikey=` in its query string. The id goes
// into the database, into API responses and into §63's stored explanations, so
// a candidate id derived from that URL would scatter the key across all three
// — which is the reason candidateID hashes the guid, and the reason that
// hashing left nothing to fetch with.
//
// Asserted against the real captured feed rather than a constructed one,
// because the value being guarded against is one a real server actually sends.
func TestACredentialInASourceDoesNotReachTheCandidateID(t *testing.T) {
	got, err := searchAgainst(t, "prowlarr", "search-with-results",
		providers.Query{Title: "ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no candidates, so this asserts nothing")
	}

	var withKey int
	for _, c := range got {
		if !strings.Contains(c.Source.Reveal(), "apikey=") {
			continue
		}
		withKey++
		if strings.Contains(c.ID, "apikey") {
			t.Errorf("%q has an api key in its id: %s", c.Title, c.ID)
		}
		if strings.Contains(c.Title, "apikey") {
			t.Errorf("%q has an api key in its title", c.Title)
		}
	}
	// The control. Without it this test passes on a corpus whose URLs stopped
	// carrying a key, while claiming to guard something it never saw.
	if withKey == 0 {
		t.Fatal("no captured source carries an api key, so the assertion is vacuous")
	}
}

// The preference order is asserted directly, because the fallbacks are only
// reachable on feeds the corpora do not contain.
//
// The magneturl case is deliberately NOT first in this table. With it first,
// "preferred the magnet attr" and "took whatever the first branch returned"
// would be the same sequence — the position-zero fixture mistake this
// repository has now found three times.
func TestSourcePreferenceOrder(t *testing.T) {
	const (
		magnet    = "magnet:?xt=urn:btih:abc"
		enclosure = "http://indexer.invalid/download?apikey=k&id=1"
		page      = "http://indexer.invalid/details/1"
	)

	attr := func(name, value string) struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} {
		return struct {
			Name  string `xml:"name,attr"`
			Value string `xml:"value,attr"`
		}{Name: name, Value: value}
	}

	withEnclosure := func(i *item, url string) { i.Enclosure.URL = url }

	for _, tc := range []struct {
		name string
		item func() item
		want string
	}{
		{
			name: "the enclosure is used when there is no magneturl attr",
			item: func() item {
				var i item
				withEnclosure(&i, enclosure)
				return i
			},
			want: enclosure,
		},
		{
			name: "a magneturl attr wins over the enclosure",
			item: func() item {
				i := item{Attrs: []struct {
					Name  string `xml:"name,attr"`
					Value string `xml:"value,attr"`
				}{attr("magneturl", magnet)}}
				withEnclosure(&i, enclosure)
				return i
			},
			want: magnet,
		},
		{
			name: "a magnet guid is used when nothing else is offered",
			item: func() item { return item{GUID: magnet} },
			want: magnet,
		},
		{
			name: "an ordinary guid is NOT a source, because it is a web page",
			item: func() item { return item{GUID: page} },
			want: "",
		},
		{
			name: "nothing fetchable at all",
			item: func() item { return item{} },
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceOf(tc.item()).Reveal(); got != tc.want {
				t.Errorf("sourceOf = %q, want %q", got, tc.want)
			}
		})
	}
}
