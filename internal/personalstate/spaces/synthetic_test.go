package spaces_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

// Adversarial synthetic tests for the spaces package. Two properties carry the
// weight: the kind gate is a CLOSED set (only the four §39 kinds pass, and
// nothing that merely looks like one), and the space id is OPAQUE — §38 lets a
// peer see that a space exists and its kind, but the id must leak nothing about
// the kind, a name, or any content. The threat model is a peer (or the
// controller-side MCP) that holds every space id it stores and tries to learn
// what a space IS from its handle.

var synthClock = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// TestKindGateRejectsLookalikes: the validator is exact-match, so case variants,
// whitespace-padded values, prefixes and empty are all refused — a kind is one of
// exactly four strings, not "close enough".
func TestKindGateRejectsLookalikes(t *testing.T) {
	t.Parallel()
	bad := []spaces.Kind{
		"", " ", "Personal", "PERSONAL", "personal ", " personal", "person",
		"personalx", "family\n", "shared\t", "reséarch", "public", "vault",
	}
	for _, k := range bad {
		if _, err := spaces.NewSpace(k, synthClock); !errors.Is(err, spaces.ErrUnknownKind) {
			t.Fatalf("NewSpace(%q) was accepted or wrong error: %v", k, err)
		}
	}
	// The four real kinds are accepted — the gate is not simply refusing everything.
	for _, k := range []spaces.Kind{spaces.KindPersonal, spaces.KindFamily, spaces.KindShared, spaces.KindResearch} {
		if _, err := spaces.NewSpace(k, synthClock); err != nil {
			t.Fatalf("NewSpace(%q) refused a real kind: %v", k, err)
		}
	}
}

// TestIDLeaksNothingAboutTheKind is the §38 opacity assertion under adversary
// pressure: mint many spaces of every kind at ONE frozen instant, and no id may
// contain its kind string, share a kind-correlated prefix, or collide — even
// though the only thing differing between two same-kind spaces is randomness.
func TestIDLeaksNothingAboutTheKind(t *testing.T) {
	t.Parallel()
	seen := make(map[string]spaces.Kind)
	kinds := []spaces.Kind{spaces.KindPersonal, spaces.KindFamily, spaces.KindShared, spaces.KindResearch}

	for _, k := range kinds {
		for i := 0; i < 200; i++ {
			sp, err := spaces.NewSpace(k, synthClock)
			if err != nil {
				t.Fatalf("NewSpace(%q): %v", k, err)
			}
			// No collisions, even at a frozen clock and same kind — which is the
			// teeth of the opacity check: if the id were DERIVED from the kind (the
			// sabotage note's uuid.NewSHA1(kind)), every same-kind id would be
			// identical and this would fire on the second space of a kind.
			if prev, dup := seen[sp.ID]; dup {
				t.Fatalf("id %q reused (kinds %q and %q): the id is derived from the kind, not opaque", sp.ID, prev, k)
			}
			seen[sp.ID] = k
			// And the id must not embed the kind string.
			if strings.Contains(sp.ID, string(k)) {
				t.Fatalf("id %q contains its kind %q — the handle leaks what the space is", sp.ID, k)
			}
		}
	}
}

// TestZeroSpaceIsNotValid: the zero value is not a usable space — it carries an
// empty id and an unknown kind, and nothing should mistake it for a real one.
func TestZeroSpaceIsNotValid(t *testing.T) {
	t.Parallel()
	var z spaces.EncryptedSpace
	if z.ID != "" {
		t.Fatalf("zero space has a non-empty id %q", z.ID)
	}
	if err := z.Kind.Validate(); !errors.Is(err, spaces.ErrUnknownKind) {
		t.Fatalf("the zero space's kind validated: %v", err)
	}
}

// TestCreatedAtIsTheInjectedClock: the timestamp comes from the caller's clock,
// normalised to UTC — never the wall clock — so two spaces minted at one instant
// share it and a test can pin it. A non-UTC input is stored as UTC.
func TestCreatedAtIsTheInjectedClock(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("somewhere", 8*3600)
	local := time.Date(2026, 8, 26, 20, 0, 0, 0, loc)
	sp, err := spaces.NewSpace(spaces.KindPersonal, local)
	if err != nil {
		t.Fatalf("NewSpace: %v", err)
	}
	if !sp.CreatedAt.Equal(local) {
		t.Fatalf("created_at %v is not the injected instant %v", sp.CreatedAt, local)
	}
	if sp.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at kept a non-UTC location: %v", sp.CreatedAt.Location())
	}
}
