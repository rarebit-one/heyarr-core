package vaultref_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultref"
)

// contract is the referring system's pattern for a vault ref, verbatim. Parse
// must never accept a string it refuses.
var contract = regexp.MustCompile(`^hv1:[0-9a-f-]{36}(/[0-9a-f-]{36})?$`)

const (
	space  = "0192f3a4-5b6c-7d8e-9f01-23456789abcd"
	object = "4f0e2c1a-9b8d-4c7e-a6f5-0123456789ab"
)

func TestParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in         string
		ok         bool
		space, obj string
	}{
		{"hv1:" + space, true, space, ""},
		{"hv1:" + space + "/" + object, true, space, object},
		{"hv1:" + strings.ToUpper(space), false, "", ""},
		{"hv1:" + space + "/", false, "", ""},
		{"hv2:" + space, false, "", ""},
		{"hv1:" + space + "\nplaintext", false, "", ""},
		{"hv1:------------------------------------", false, "", ""}, // the contract's charset, not a UUID
		{"hv1:" + strings.ReplaceAll(space, "-", "") + "0000", false, "", ""},
		{space, false, "", ""},
		{"", false, "", ""},
	} {
		r, err := vaultref.Parse(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("Parse(%q) error = %v, want ok=%v", tc.in, err, tc.ok)
			continue
		}
		if tc.ok {
			if !contract.MatchString(tc.in) {
				t.Errorf("Parse accepted %q, which the contract refuses", tc.in)
			}
			if r.Space != tc.space || r.Object != tc.obj || r.String() != tc.in {
				t.Errorf("Parse(%q) = %+v (%s)", tc.in, r, r)
			}
		}
	}
}

func TestNewAndPath(t *testing.T) {
	t.Parallel()
	r, err := vaultref.New(space)
	if err != nil {
		t.Fatal(err)
	}
	if !contract.MatchString(r.String()) {
		t.Errorf("New minted %s, which the contract refuses", r)
	}
	back, err := vaultref.Parse(r.String())
	if err != nil || back != r {
		t.Errorf("a minted ref does not round-trip: %+v, %v", back, err)
	}
	if r.Object[14] != '4' {
		t.Errorf("an object id must be random (v4), carrying no time: %s", r.Object)
	}
	if want := ".jumpdrive/objects/" + r.Object + ".json"; r.Path() != want {
		t.Errorf("Path = %q, want %q", r.Path(), want)
	}
	if (vaultref.Ref{Space: space}).Path() != "" {
		t.Error("a collection ref has a drive path")
	}
	if _, err := vaultref.New("not-a-space"); err == nil {
		t.Error("New accepted a malformed space id")
	}
}

func TestParseSpace(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{space: space, "hv1:" + space: space} {
		if got, err := vaultref.ParseSpace(in); err != nil || got != want {
			t.Errorf("ParseSpace(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"hv1:" + space + "/" + object, "x", strings.ToUpper(space)} {
		if _, err := vaultref.ParseSpace(in); err == nil {
			t.Errorf("ParseSpace(%q) was accepted", in)
		}
	}
}
