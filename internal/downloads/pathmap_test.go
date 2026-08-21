package downloads

import (
	"strings"
	"testing"
)

// Path mapping — the most common operational failure in this class of software.

func TestPathMapResolves(t *testing.T) {
	maps := []Mapping{
		{Remote: "/downloads", Local: "/srv/dl"},
		// Deliberately listed AFTER the shorter prefix, to prove ordering is
		// by specificity rather than by the order an operator happened to
		// write them.
		{Remote: "/downloads/complete", Local: "/srv/media/incoming"},
	}
	pm, err := ParsePathMap("test", maps)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, in, want string
		mapped         bool
	}{
		{
			name:   "the more specific prefix wins regardless of configured order",
			in:     "/downloads/complete/Film.mkv",
			want:   "/srv/media/incoming/Film.mkv",
			mapped: true,
		},
		{
			name:   "the general prefix still applies elsewhere",
			in:     "/downloads/incomplete/Film.mkv",
			want:   "/srv/dl/incomplete/Film.mkv",
			mapped: true,
		},
		{
			name:   "an exact match maps to the local root",
			in:     "/downloads",
			want:   "/srv/dl",
			mapped: true,
		},
		{
			// The separator check. `/downloads-old` is a real directory name
			// and must not be rewritten into somewhere it has no business
			// being — a prefix match without it would produce
			// "/srv/dl-old", silently.
			name:   "a prefix that is not a path boundary does not match",
			in:     "/downloads-old/Film.mkv",
			mapped: false,
		},
		{
			name:   "an unrelated path does not match",
			in:     "/var/lib/other/Film.mkv",
			mapped: false,
		},
		{
			name:   "traversal is cleaned before matching",
			in:     "/downloads/complete/../complete/Film.mkv",
			want:   "/srv/media/incoming/Film.mkv",
			mapped: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pm.Resolve(tc.in)
			if ok != tc.mapped {
				t.Fatalf("Resolve(%q) mapped = %v, want %v (got %q)", tc.in, ok, tc.mapped, got)
			}
			if tc.mapped && got != tc.want {
				t.Errorf("Resolve(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// An empty map maps nothing, and says so rather than returning the identity.
//
// The distinction matters: a caller has to decide whether an unmapped path
// means "the client and Heyarr share a filesystem" or "this is misconfigured",
// and collapsing them into a silent identity produces the exact failure this
// file exists to prevent.
func TestAnEmptyMapMapsNothing(t *testing.T) {
	var pm PathMap
	if got, ok := pm.Resolve("/downloads/x"); ok {
		t.Errorf("an empty map resolved to %q", got)
	}
}

func TestParsePathMapRefusals(t *testing.T) {
	cases := []struct {
		name string
		in   []Mapping
		want string
	}{
		{"no remote", []Mapping{{Local: "/srv"}}, "no remote prefix"},
		{"no local", []Mapping{{Remote: "/downloads"}}, "no local prefix"},
		{
			"a relative remote",
			[]Mapping{{Remote: "downloads", Local: "/srv"}},
			"must be an absolute path",
		},
		{
			"a relative local",
			[]Mapping{{Remote: "/downloads", Local: "srv"}},
			"must be an absolute path",
		},
		{
			// One of them would never apply, and which is an accident of
			// ordering.
			"the same prefix twice",
			[]Mapping{{Remote: "/downloads", Local: "/a"}, {Remote: "/downloads", Local: "/b"}},
			"twice",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePathMap("acquire", tc.in)
			if err == nil {
				t.Fatal("this mapping should be refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal should mention %q, said: %v", tc.want, err)
			}
			// Every message names the provider, because an instance with
			// several produces an error somebody has to act on without
			// reading the source.
			if !strings.Contains(err.Error(), "acquire") {
				t.Errorf("the refusal should name the provider, said: %v", err)
			}
		})
	}
}

// Ordering is by specificity, and it is stable — it appears in a log line and
// an order that depends on map iteration is one nobody can diff.
func TestOrderingIsByPrefixLengthAndStable(t *testing.T) {
	in := []Mapping{
		{Remote: "/a", Local: "/1"},
		{Remote: "/a/b/c", Local: "/3"},
		{Remote: "/a/b", Local: "/2"},
	}
	for range 20 {
		pm, err := ParsePathMap("test", in)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(pm))
		for i, m := range pm {
			got[i] = m.Remote
		}
		if strings.Join(got, ",") != "/a/b/c,/a/b,/a" {
			t.Fatalf("order = %v, want longest prefix first", got)
		}
	}
}
