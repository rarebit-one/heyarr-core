package protocol_test

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

// chain builds n linked changes a←b←c… in space s, returning them and the ids.
func chain(t *testing.T, s string, n int) ([]protocol.EncryptedChange, []string) {
	t.Helper()
	var out []protocol.EncryptedChange
	var ids []string
	var parents []string
	for i := 0; i < n; i++ {
		c, err := protocol.NewChange(s, parents, []byte{byte(i), 0x5a})
		if err != nil {
			t.Fatalf("NewChange: %v", err)
		}
		out = append(out, c)
		ids = append(ids, c.ChangeID)
		parents = []string{c.ChangeID}
	}
	return out, ids
}

func idsOf(cs []protocol.EncryptedChange) map[string]bool {
	m := make(map[string]bool, len(cs))
	for _, c := range cs {
		m[c.ChangeID] = true
	}
	return m
}

// TestMissingKnowsNothing: a peer that offers no heads is missing everything.
func TestMissingKnowsNothing(t *testing.T) {
	t.Parallel()
	have, _ := chain(t, "s", 5)
	missing := protocol.Missing(have, nil)
	if len(missing) != 5 {
		t.Fatalf("a peer knowing nothing is missing %d, want 5", len(missing))
	}
}

// TestMissingKnowsTheHead: a peer that already holds the tip head is missing
// nothing — the head's whole ancestry is known.
func TestMissingKnowsTheHead(t *testing.T) {
	t.Parallel()
	have, ids := chain(t, "s", 5)
	if missing := protocol.Missing(have, []string{ids[4]}); len(missing) != 0 {
		t.Fatalf("a peer holding the head is missing %d, want 0", len(missing))
	}
}

// TestMissingKnowsAMidpoint: a peer that holds a mid change is missing only what
// is beyond it — the incremental pull.
func TestMissingKnowsAMidpoint(t *testing.T) {
	t.Parallel()
	have, ids := chain(t, "s", 5) // 0←1←2←3←4
	missing := protocol.Missing(have, []string{ids[2]})
	got := idsOf(missing)
	if len(got) != 2 || !got[ids[3]] || !got[ids[4]] {
		t.Fatalf("midpoint missing = %v, want the two beyond ids[2]", missing)
	}
}

// TestMissingAcrossAFork: knowing one fork tip still leaves the other fork's tip
// missing.
func TestMissingAcrossAFork(t *testing.T) {
	t.Parallel()
	root, err := protocol.NewChange("s", nil, []byte("root"))
	if err != nil {
		t.Fatal(err)
	}
	left, _ := protocol.NewChange("s", []string{root.ChangeID}, []byte("left"))
	right, _ := protocol.NewChange("s", []string{root.ChangeID}, []byte("right"))
	have := []protocol.EncryptedChange{root, left, right}

	missing := protocol.Missing(have, []string{left.ChangeID})
	got := idsOf(missing)
	if len(got) != 1 || !got[right.ChangeID] {
		t.Fatalf("knowing the left tip, missing = %v, want [right]", missing)
	}
}

// TestMissingIgnoresUnknownHeads: a head the holder does not have (the offering
// peer is ahead) cannot be walked, so it marks nothing known — the holder still
// offers everything it has, biasing toward sending more, never less.
func TestMissingIgnoresUnknownHeads(t *testing.T) {
	t.Parallel()
	have, _ := chain(t, "s", 3)
	missing := protocol.Missing(have, []string{"blake3:deadbeef"})
	if len(missing) != 3 {
		t.Fatalf("an unknown head suppressed changes: missing %d, want 3", len(missing))
	}
}

// TestMissingIsDeterministic: the same have + heads always yield the same,
// sorted, result — two holders answer one offer identically.
func TestMissingIsDeterministic(t *testing.T) {
	t.Parallel()
	have, _ := chain(t, "s", 6)
	first := protocol.Missing(have, nil)
	for i := 0; i < 20; i++ {
		got := protocol.Missing(have, nil)
		if len(got) != len(first) {
			t.Fatal("Missing is not deterministic in length")
		}
		for j := range got {
			if got[j].ChangeID != first[j].ChangeID {
				t.Fatalf("Missing order differs at %d", j)
			}
		}
	}
}

// TestHaveAllReportsGapsAndCompleteness: HaveAll is true when the whole ancestry
// of the wanted heads is present, and names the first missing id otherwise.
func TestHaveAllReportsGapsAndCompleteness(t *testing.T) {
	t.Parallel()
	have, ids := chain(t, "s", 4)

	if ok, gap := protocol.HaveAll(have, []string{ids[3]}); !ok {
		t.Fatalf("HaveAll over a complete chain = false (gap %s)", gap)
	}
	// Drop the middle change; wanting the head is now incomplete.
	partial := []protocol.EncryptedChange{have[0], have[1], have[3]} // missing ids[2]
	ok, gap := protocol.HaveAll(partial, []string{ids[3]})
	if ok {
		t.Fatal("HaveAll said caught-up over a chain with a hole")
	}
	if gap != ids[2] {
		t.Fatalf("HaveAll named gap %s, want %s", gap, ids[2])
	}
}
