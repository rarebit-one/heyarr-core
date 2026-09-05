package catalogop_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
)

var iat = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

// peer mints a signing key and helpers to sign catalog ops as one site.
type peer struct {
	t    *testing.T
	priv ed25519.PrivateKey
}

func newPeer(t *testing.T) *peer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &peer{t: t, priv: priv}
}

func (p *peer) sign(kind catalogop.Kind, ct, wk string, prev []string, at time.Time) string {
	p.t.Helper()
	tok, err := catalogop.Sign(p.priv, kind, ct, wk, prev, at)
	if err != nil {
		p.t.Fatalf("sign %s %s/%s: %v", kind, ct, wk, err)
	}
	return tok
}

func (p *peer) del(ct, wk string, prev []string, at time.Time) string {
	return p.sign(catalogop.OpDelete, ct, wk, prev, at)
}

func (p *peer) restore(ct, wk string, prev []string, at time.Time) string {
	return p.sign(catalogop.OpRestore, ct, wk, prev, at)
}

func target(ct, wk string) catalogop.Target { return catalogop.Target{ContentType: ct, WorkKey: wk} }

// TestSignVerifyRoundTrip: a signed op parses back to the fields it carried.
func TestSignVerifyRoundTrip(t *testing.T) {
	p := newPeer(t)
	tok := p.del("movie", "the-thing-1982", nil, iat)
	op, err := catalogop.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if op.Kind != catalogop.OpDelete {
		t.Errorf("kind = %q, want delete", op.Kind)
	}
	if op.Target != target("movie", "the-thing-1982") {
		t.Errorf("target = %+v", op.Target)
	}
	if op.Hash != catalogop.Hash(tok) {
		t.Errorf("hash mismatch")
	}
	if !op.IssuedAt.Equal(iat) {
		t.Errorf("iat = %v, want %v", op.IssuedAt, iat)
	}
}

// TestVerifyRejectsTamperedPayload: flipping a byte of the signed body fails
// the signature, and swapping the target for another peer's fails too.
func TestVerifyRejectsTamperedPayload(t *testing.T) {
	p := newPeer(t)
	tok := p.del("movie", "alien-1979", nil, iat)

	// Corrupt the signature half — by flipping a character to one it is not,
	// so the tamper is never a no-op (overwriting the tail with "AA" left the
	// token intact whenever the signature's last byte was already zero).
	if _, err := catalogop.Verify(flipSignatureChar(tok)); err == nil {
		t.Error("expected a bad signature to be refused")
	}
	// Not an op at all.
	if _, err := catalogop.Verify("not-a-token"); err == nil {
		t.Error("expected a malformed token to be refused")
	}
	// A payload re-signed by a DIFFERENT key does not verify under the original
	// `by` — decode the body, re-sign with a stranger, reassemble.
	stranger := newPeer(t)
	dot := -1
	for i := 0; i < len(tok); i++ {
		if tok[i] == '.' {
			dot = i
			break
		}
	}
	body, err := base64.RawURLEncoding.DecodeString(tok[:dot])
	if err != nil {
		t.Fatal(err)
	}
	forged := tok[:dot+1] + base64.RawURLEncoding.EncodeToString(ed25519.Sign(stranger.priv, body))
	if _, err := catalogop.Verify(forged); err == nil {
		t.Error("expected a signature by the wrong key to be refused")
	}
}

// TestNoOpsIsLive: with no ops, nothing is tombstoned — the base spine
// re-derives everything.
func TestNoOpsIsLive(t *testing.T) {
	v := catalogop.Evaluate(nil)
	if len(v.Tombstoned) != 0 {
		t.Errorf("empty log tombstoned %d works", len(v.Tombstoned))
	}
}

// TestDeleteTombstones: a single delete tombstones its work and nothing else.
func TestDeleteTombstones(t *testing.T) {
	p := newPeer(t)
	v := catalogop.Evaluate([]string{p.del("movie", "the-thing-1982", nil, iat)})
	if !v.IsTombstoned(target("movie", "the-thing-1982")) {
		t.Error("the deleted work is not tombstoned")
	}
	if v.IsTombstoned(target("movie", "alien-1979")) {
		t.Error("an unrelated work is tombstoned")
	}
}

// TestRemoveWins is the core Phase-1 guarantee: a restore CONCURRENT with the
// delete (it does not cite the delete) does not resurrect the work.
func TestRemoveWins(t *testing.T) {
	siteA := newPeer(t)
	siteB := newPeer(t)

	del := siteA.del("movie", "the-thing-1982", nil, iat)
	// site B, not having seen the delete, signs a restore citing nothing.
	restoreConcurrent := siteB.restore("movie", "the-thing-1982", nil, iat.Add(time.Minute))

	v := catalogop.Evaluate([]string{del, restoreConcurrent})
	if !v.IsTombstoned(target("movie", "the-thing-1982")) {
		t.Error("a concurrent restore resurrected the work — remove-wins violated")
	}
}

// TestCausalRestoreLiftsTombstone: a restore that CITES the delete (has seen it)
// lifts the tombstone — the re-ripped-months-later case (ADR-0073 open Q4).
func TestCausalRestoreLiftsTombstone(t *testing.T) {
	p := newPeer(t)
	del := p.del("movie", "the-thing-1982", nil, iat)
	delHash := catalogop.Hash(del)
	restore := p.restore("movie", "the-thing-1982", []string{delHash}, iat.Add(time.Hour))

	v := catalogop.Evaluate([]string{del, restore})
	if v.IsTombstoned(target("movie", "the-thing-1982")) {
		t.Error("a causal restore did not lift the tombstone")
	}
}

// TestReDeleteAfterRestore: delete → restore(cites delete) → delete(cites
// restore) tombstones again. A later delete citing the restore is not
// overridden by that earlier restore.
func TestReDeleteAfterRestore(t *testing.T) {
	p := newPeer(t)
	del1 := p.del("movie", "the-thing-1982", nil, iat)
	restore := p.restore("movie", "the-thing-1982", []string{catalogop.Hash(del1)}, iat.Add(time.Hour))
	del2 := p.del("movie", "the-thing-1982", []string{catalogop.Hash(restore)}, iat.Add(2*time.Hour))

	v := catalogop.Evaluate([]string{del1, restore, del2})
	if !v.IsTombstoned(target("movie", "the-thing-1982")) {
		t.Error("a re-delete citing the restore did not tombstone again")
	}
}

// TestBadPrevIsUncitable: a restore citing a delete NOT in the set is rejected
// (bad_prev), so it cannot lift a tombstone learned later. Here the delete IS
// present but the restore cites a hash nobody signed.
func TestBadPrevIsUncitable(t *testing.T) {
	p := newPeer(t)
	del := p.del("movie", "the-thing-1982", nil, iat)
	restore := p.restore("movie", "the-thing-1982", []string{"sha256:deadbeef"}, iat.Add(time.Hour))

	v := catalogop.Evaluate([]string{del, restore})
	if v.Rejected[catalogop.Hash(restore)] != catalogop.ReasonBadPrev {
		t.Errorf("restore citing a missing prev = %q, want bad_prev", v.Rejected[catalogop.Hash(restore)])
	}
	if !v.IsTombstoned(target("movie", "the-thing-1982")) {
		t.Error("an uncitable restore lifted the tombstone")
	}
}

// TestMergeIsSetUnion: Merge de-duplicates by hash and is order-independent, so
// two sites that merge the same ops in any order hold the same set.
func TestMergeIsSetUnion(t *testing.T) {
	p := newPeer(t)
	a := p.del("movie", "alien-1979", nil, iat)
	b := p.del("movie", "the-thing-1982", nil, iat)

	m1 := catalogop.Merge([]string{a, b}, []string{b}) // duplicate b
	m2 := catalogop.Merge([]string{b}, []string{a, a}) // duplicate a, other order
	if len(m1) != 2 || len(m2) != 2 {
		t.Fatalf("merge kept duplicates: %d, %d", len(m1), len(m2))
	}
	for i := range m1 {
		if m1[i] != m2[i] {
			t.Errorf("merge is not order-independent at %d: %q vs %q", i, m1[i], m2[i])
		}
	}
}

// TestEvaluateIsOrderIndependent: every permutation of a scenario's ops
// evaluates to the same tombstone set — the CRDT property membership pins.
func TestEvaluateIsOrderIndependent(t *testing.T) {
	p := newPeer(t)
	del := p.del("movie", "the-thing-1982", nil, iat)
	restore := p.restore("movie", "the-thing-1982", []string{catalogop.Hash(del)}, iat.Add(time.Hour))
	other := p.del("series", "the-wire", nil, iat)
	ops := []string{del, restore, other}

	want := tombstoneSet(catalogop.Evaluate(ops))
	for _, perm := range permutations(ops) {
		got := tombstoneSet(catalogop.Evaluate(perm))
		if !sameSet(want, got) {
			t.Fatalf("order %v changed the tombstone set: %v vs %v", perm, want, got)
		}
	}
}

// TestPartitionThenMergeConverges is the two-site story: site A deletes, site B
// (partitioned) still has the work live, and once they exchange ops BOTH sites
// evaluate to the same tombstoned set. Neither the delete-only side nor the
// merged side disagrees.
func TestPartitionThenMergeConverges(t *testing.T) {
	siteA := newPeer(t)
	siteB := newPeer(t)

	// During the partition: A deleted a work; B independently deleted another.
	aOps := []string{siteA.del("movie", "the-thing-1982", nil, iat)}
	bOps := []string{siteB.del("series", "the-wire", nil, iat)}

	// Before exchange the sites disagree, as they must.
	if catalogop.Evaluate(aOps).IsTombstoned(target("series", "the-wire")) {
		t.Error("site A tombstoned a work it never saw deleted")
	}

	// After exchange (merge in either direction) they agree.
	mergedAB := catalogop.Merge(aOps, bOps)
	mergedBA := catalogop.Merge(bOps, aOps)
	va := catalogop.Evaluate(mergedAB)
	vb := catalogop.Evaluate(mergedBA)

	for _, tg := range []catalogop.Target{target("movie", "the-thing-1982"), target("series", "the-wire")} {
		if !va.IsTombstoned(tg) || !vb.IsTombstoned(tg) {
			t.Errorf("post-merge sites disagree on %+v: A=%v B=%v", tg, va.IsTombstoned(tg), vb.IsTombstoned(tg))
		}
	}
	if !sameSet(tombstoneSet(va), tombstoneSet(vb)) {
		t.Error("merge is not commutative")
	}
}

// --- helpers ---

func tombstoneSet(v catalogop.View) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range v.Tombstoned {
		out[k] = struct{}{}
	}
	return out
}

func sameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func permutations(in []string) [][]string {
	var out [][]string
	var rec func(cur, rest []string)
	rec = func(cur, rest []string) {
		if len(rest) == 0 {
			cp := make([]string, len(cur))
			copy(cp, cur)
			out = append(out, cp)
			return
		}
		for i := range rest {
			next := make([]string, 0, len(rest)-1)
			next = append(next, rest[:i]...)
			next = append(next, rest[i+1:]...)
			rec(append(cur, rest[i]), next)
		}
	}
	rec(nil, in)
	return out
}

// flipSignatureChar changes one character in the middle of the signature half
// of a token to a different valid base64url character.
func flipSignatureChar(tok string) string {
	dot := strings.IndexByte(tok, '.')
	i := dot + 1 + (len(tok)-dot-1)/2
	c := byte('A')
	if tok[i] == 'A' {
		c = 'B'
	}
	return tok[:i] + string(c) + tok[i+1:]
}
