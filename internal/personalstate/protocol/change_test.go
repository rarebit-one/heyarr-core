package protocol_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

func mustChange(t *testing.T, space string, parents []string, ct []byte) protocol.EncryptedChange {
	t.Helper()
	c, err := protocol.NewChange(space, parents, ct)
	if err != nil {
		t.Fatalf("NewChange: %v", err)
	}
	return c
}

// TestChangeIDIsDeterministicAndContentAddressed: the same space, parents and
// ciphertext always produce the same id — two peers agree without coordination —
// and the id is a "blake3:<hex>".
func TestChangeIDIsDeterministicAndContentAddressed(t *testing.T) {
	t.Parallel()
	a := mustChange(t, "space-1", []string{"p2", "p1"}, []byte("ciphertext"))
	b := mustChange(t, "space-1", []string{"p1", "p2"}, []byte("ciphertext"))
	if a.ChangeID != b.ChangeID {
		t.Fatalf("id depends on parent order: %s vs %s", a.ChangeID, b.ChangeID)
	}
	if !strings.HasPrefix(a.ChangeID, "blake3:") {
		t.Fatalf("id is not a blake3 hash: %s", a.ChangeID)
	}
	// Duplicate parents do not change the id.
	c := mustChange(t, "space-1", []string{"p1", "p2", "p2", "p1"}, []byte("ciphertext"))
	if c.ChangeID != a.ChangeID {
		t.Fatal("duplicate parents changed the id")
	}
}

// TestValidateRejectsAClaimedID is Invariant 1 for changes: a destination never
// trusts a claimed id. Tampering any field a peer can see — the stated id, the
// space, a parent, the ciphertext — makes Validate refuse, because the id no
// longer matches the content.
func TestValidateRejectsAClaimedID(t *testing.T) {
	t.Parallel()
	good := mustChange(t, "space-1", []string{"p1"}, []byte("ciphertext"))
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed change was refused: %v", err)
	}

	// Each mutation is a change a malicious peer might forge; each must fail id.
	mutations := []struct {
		name string
		c    protocol.EncryptedChange
	}{
		{"forged id", protocol.EncryptedChange{SpaceID: good.SpaceID, ChangeID: "blake3:" + strings.Repeat("00", 32), Parents: good.Parents, Ciphertext: good.Ciphertext}},
		{"re-pointed space", protocol.EncryptedChange{SpaceID: "space-2", ChangeID: good.ChangeID, Parents: good.Parents, Ciphertext: good.Ciphertext}},
		{"re-parented", protocol.EncryptedChange{SpaceID: good.SpaceID, ChangeID: good.ChangeID, Parents: []string{"other"}, Ciphertext: good.Ciphertext}},
		{"tampered ciphertext", protocol.EncryptedChange{SpaceID: good.SpaceID, ChangeID: good.ChangeID, Parents: good.Parents, Ciphertext: []byte("different")}},
	}
	for _, m := range mutations {
		if err := m.c.Validate(); !errors.Is(err, protocol.ErrIDMismatch) {
			t.Fatalf("%s: Validate = %v, want ErrIDMismatch", m.name, err)
		}
	}
}

// TestSpaceIsBoundIntoTheID: the same ciphertext in a different space gets a
// different id — a change cannot be re-pointed at another space and keep its id.
func TestSpaceIsBoundIntoTheID(t *testing.T) {
	t.Parallel()
	a := mustChange(t, "space-a", nil, []byte("same"))
	b := mustChange(t, "space-b", nil, []byte("same"))
	if a.ChangeID == b.ChangeID {
		t.Fatal("the space is not bound into the change id")
	}
}

// TestFramingPreventsFieldMigration: length-framing means a byte cannot migrate
// from the end of the space id into the first parent (or anywhere) and leave the
// id unchanged. The two changes below have the same raw concatenation but a
// different framed structure, so their ids must differ.
func TestFramingPreventsFieldMigration(t *testing.T) {
	t.Parallel()
	// "space-a" ‖ "p1"  vs  "space-" ‖ "ap1"  — same bytes, different boundaries.
	a := mustChange(t, "space-a", []string{"p1"}, []byte("ct"))
	b := mustChange(t, "space-", []string{"ap1"}, []byte("ct"))
	if a.ChangeID == b.ChangeID {
		t.Fatal("a boundary migration produced the same id: framing is broken")
	}
}

// TestHeads finds the causal frontier: a chain has one head, a fork has both tips,
// and concurrent roots are both heads.
func TestHeads(t *testing.T) {
	t.Parallel()
	root := mustChange(t, "s", nil, []byte("a"))
	mid := mustChange(t, "s", []string{root.ChangeID}, []byte("b"))
	tip := mustChange(t, "s", []string{mid.ChangeID}, []byte("c"))

	// A ← B ← C: only C is a head.
	if heads := protocol.Heads([]protocol.EncryptedChange{root, mid, tip}); len(heads) != 1 || heads[0] != tip.ChangeID {
		t.Fatalf("chain heads = %v, want [%s]", heads, tip.ChangeID)
	}

	// A ← B, A ← C': a fork, B and C' are both heads.
	fork := mustChange(t, "s", []string{root.ChangeID}, []byte("c-prime"))
	heads := protocol.Heads([]protocol.EncryptedChange{root, mid, fork})
	if len(heads) != 2 {
		t.Fatalf("fork heads = %v, want 2", heads)
	}

	// Two independent roots are both heads.
	root2 := mustChange(t, "s", nil, []byte("z"))
	if h := protocol.Heads([]protocol.EncryptedChange{root, root2}); len(h) != 2 {
		t.Fatalf("two roots heads = %v, want 2", h)
	}
}

// TestNewChangeRejectsEmpty: a change with no space or no ciphertext is refused —
// it references nothing and merges nothing.
func TestNewChangeRejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := protocol.NewChange("", nil, []byte("x")); !errors.Is(err, protocol.ErrIncomplete) {
		t.Fatalf("empty space = %v, want ErrIncomplete", err)
	}
	if _, err := protocol.NewChange("s", nil, nil); !errors.Is(err, protocol.ErrIncomplete) {
		t.Fatalf("empty ciphertext = %v, want ErrIncomplete", err)
	}
}

// TestJSONRoundTrip: a change encodes and decodes losslessly, and the decoded
// change still validates — the wire form a peer sends and stores.
func TestJSONRoundTrip(t *testing.T) {
	t.Parallel()
	c := mustChange(t, "space-1", []string{"p1", "p2"}, []byte{0x00, 0x01, 0xff, 0x5a})
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back protocol.EncryptedChange
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.ChangeID != c.ChangeID || back.SpaceID != c.SpaceID {
		t.Fatal("json round trip changed a field")
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("a round-tripped change did not validate: %v", err)
	}
}
