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
// ordered, result — two holders answer one offer identically.
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

// childBeforeParent builds a parent and a child whose id sorts BEFORE the
// parent's, the case an id-sorted Missing got wrong (#682).
func childBeforeParent(t *testing.T, s string) (parent, child protocol.EncryptedChange) {
	t.Helper()
	for i := 0; i < 256; i++ {
		p, err := protocol.NewChange(s, nil, []byte{byte(i), 0x01})
		if err != nil {
			t.Fatal(err)
		}
		c, err := protocol.NewChange(s, []string{p.ChangeID}, []byte{byte(i), 0x02})
		if err != nil {
			t.Fatal(err)
		}
		if c.ChangeID < p.ChangeID {
			return p, c
		}
	}
	t.Fatal("no child id sorted before its parent's in 256 tries")
	return
}

// TestMissingIsTopological: Missing answers every change after its parents, even
// when a child's id sorts first, whatever order the holder lists its changes in
// (#682, ADR-0101). A pusher stopping at its first failure then never strands a
// child without its parent on the target.
func TestMissingIsTopological(t *testing.T) {
	t.Parallel()
	p, c := childBeforeParent(t, "s")
	other, err := protocol.NewChange("s", nil, []byte("other root"))
	if err != nil {
		t.Fatal(err)
	}
	merge, err := protocol.NewChange("s", []string{c.ChangeID, other.ChangeID}, []byte("merge"))
	if err != nil {
		t.Fatal(err)
	}
	orders := [][]protocol.EncryptedChange{
		{p, c, other, merge},
		{merge, c, other, p},
		{c, merge, p, other},
	}
	var first []string
	for _, have := range orders {
		got := protocol.Missing(have, nil)
		pos := make(map[string]int, len(got))
		var ids []string
		for i, ch := range got {
			pos[ch.ChangeID] = i
			ids = append(ids, ch.ChangeID)
		}
		if len(got) != 4 {
			t.Fatalf("Missing returned %d changes, want 4", len(got))
		}
		for _, ch := range got {
			for _, parent := range ch.Parents {
				if pos[parent] > pos[ch.ChangeID] {
					t.Fatalf("change %s came before its parent %s", ch.ChangeID, parent)
				}
			}
		}
		if first == nil {
			first = ids
			continue
		}
		for i := range ids {
			if ids[i] != first[i] {
				t.Fatalf("the order depends on how the holder listed its changes: %v vs %v", ids, first)
			}
		}
	}
}

// TestMissingOrdersAroundAParentTheTargetHolds: a parent outside the answer (the
// target holds it, or it was compacted) does not hold its child back.
func TestMissingOrdersAroundAParentTheTargetHolds(t *testing.T) {
	t.Parallel()
	have, ids := chain(t, "s", 4)
	got := protocol.Missing(have, []string{ids[1]})
	if len(got) != 2 || got[0].ChangeID != ids[2] || got[1].ChangeID != ids[3] {
		t.Fatalf("Missing beneath a known midpoint = %v, want [%s %s]", got, ids[2], ids[3])
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

// TestCovers pins the keyless rule a peer uses to replace or bypass a pinned
// compaction base (#681): the newer frontier must reach every head of the base,
// through held changes or a parent a held change names.
func TestCovers(t *testing.T) {
	t.Parallel()
	full, ids := chain(t, "s", 5) // 0←1←2←3←4
	fork, err := protocol.NewChange("s", []string{ids[1]}, []byte("fork"))
	if err != nil {
		t.Fatal(err)
	}
	all := append(append([]protocol.EncryptedChange(nil), full...), fork)
	compacted := full[3:] // 0..2 compacted away; 3 still names 2 as its parent

	for _, tc := range []struct {
		name           string
		have           []protocol.EncryptedChange
		frontier, base []string
		want           bool
	}{
		{"a point covers itself", all, []string{ids[2]}, []string{ids[2]}, true},
		{"a descendant covers its ancestor", all, []string{ids[4]}, []string{ids[2]}, true},
		{"an ancestor does not cover a descendant", all, []string{ids[2]}, []string{ids[4]}, false},
		{"a concurrent branch does not cover", all, []string{fork.ChangeID}, []string{ids[3]}, false},
		{"a merge of both branches covers each", all, []string{ids[4], fork.ChangeID}, []string{ids[3], fork.ChangeID}, true},
		{"after compaction, a surviving child's parent link proves it", compacted, []string{ids[4]}, []string{ids[2]}, true},
		{"after compaction, an ancestry it cannot walk is not covered", compacted, []string{ids[4]}, []string{ids[1]}, false},
		{"an empty base is covered", all, []string{ids[0]}, nil, true},
	} {
		if got := protocol.Covers(tc.have, tc.frontier, tc.base); got != tc.want {
			t.Errorf("%s: Covers = %v, want %v", tc.name, got, tc.want)
		}
	}
}
