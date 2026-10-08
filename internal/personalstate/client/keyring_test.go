package client_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

// rotated creates a space for r and rotates it n times, writing one change under
// each epoch's key. It returns the space id, the latest wrapped copy for r, the
// history rows, and the change written at each epoch (index = epoch).
func rotated(t *testing.T, r client.Recipient, n int) (string, []byte, []client.HistoryEntry, [][]byte) {
	t.Helper()
	m := client.New()
	sp, wrapped, err := m.Create(spaces.KindPersonal, testNow, []client.Recipient{r})
	if err != nil {
		t.Fatal(err)
	}
	latest := wrapped[0].Wrapped
	var history []client.HistoryEntry
	cts := make([][]byte, 0, n+1)
	for e := 0; ; e++ {
		ct, err := m.Encrypt(sp.ID, []byte(fmt.Sprintf("written at epoch %d", e)))
		if err != nil {
			t.Fatal(err)
		}
		cts = append(cts, ct)
		if e == n {
			break
		}
		rot, err := m.Rotate(sp.ID, []client.Recipient{r})
		if err != nil {
			t.Fatal(err)
		}
		if rot.Epoch != e+1 {
			t.Fatalf("rotation %d moved to epoch %d", e+1, rot.Epoch)
		}
		latest = rot.Wrapped[0].Wrapped
		history = append(history, client.HistoryEntry{Epoch: rot.Epoch, SealedPrev: rot.SealedPrev})
	}
	if got, _ := m.Epoch(sp.ID); got != n {
		t.Fatalf("the rotating manager is at epoch %d, want %d", got, n)
	}
	// The rotating manager itself still reads every epoch's change.
	for e, ct := range cts {
		if got, err := m.Decrypt(sp.ID, ct); err != nil || string(got) != fmt.Sprintf("written at epoch %d", e) {
			t.Fatalf("the rotating manager lost epoch %d: %q %v", e, got, err)
		}
	}
	return sp.ID, latest, history, cts
}

// TestOpenWithHistoryUnrollsEveryEpoch: a fresh device opening at epoch 3 from
// its wrap and the three history rows (served in any order) reads content from
// every epoch, encrypts under the newest key only, and exposes the ring
// newest-first for vault readers.
func TestOpenWithHistoryUnrollsEveryEpoch(t *testing.T) {
	t.Parallel()
	_, r, u := dev(t)
	spaceID, wrapped, history, cts := rotated(t, r, 3)

	shuffled := []client.HistoryEntry{history[1], history[2], history[0]}
	m := client.New()
	if err := m.OpenWithHistory(spaceID, wrapped, 3, shuffled, u); err != nil {
		t.Fatalf("OpenWithHistory: %v", err)
	}
	for e, ct := range cts {
		if got, err := m.Decrypt(spaceID, ct); err != nil || string(got) != fmt.Sprintf("written at epoch %d", e) {
			t.Fatalf("epoch %d: %q %v", e, got, err)
		}
	}
	keys, ok := m.Keys(spaceID)
	if !ok || len(keys) != 4 {
		t.Fatalf("Keys = %d keys (%v), want 4", len(keys), ok)
	}
	for i, k := range keys {
		e := 3 - i
		if _, err := encryption.DecryptChange(k, cts[e]); err != nil {
			t.Fatalf("Keys()[%d] is not the key of epoch %d", i, e)
		}
	}
	cur, _ := m.SpaceKey(spaceID)
	ct, err := m.Encrypt(spaceID, []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encryption.DecryptChange(cur, ct); err != nil {
		t.Fatal("Encrypt did not use the current key")
	}
	if _, err := encryption.DecryptChange(keys[1], ct); err == nil {
		t.Fatal("Encrypt sealed under an old key")
	}
	if e, _ := m.Epoch(spaceID); e != 3 {
		t.Fatalf("Epoch = %d, want 3", e)
	}
}

// TestOpenWithHistoryRefusesABrokenChain: a missing, duplicated, out-of-range or
// foreign row fails the open loudly, as does opening a rotated space as epoch 0
// (its content would otherwise be silently unreadable).
func TestOpenWithHistoryRefusesABrokenChain(t *testing.T) {
	t.Parallel()
	_, r, u := dev(t)
	spaceID, wrapped, history, _ := rotated(t, r, 3)
	_, _, foreign, _ := rotated(t, r, 1)

	cases := []struct {
		name    string
		epoch   int
		history []client.HistoryEntry
		want    error
	}{
		{"a missing middle row", 3, []client.HistoryEntry{history[0], history[2]}, client.ErrIncompleteHistory},
		{"no rows at all", 3, nil, client.ErrIncompleteHistory},
		{"a duplicated row", 3, []client.HistoryEntry{history[0], history[1], history[2], history[2]}, client.ErrIncompleteHistory},
		{"a row past the epoch", 2, history, client.ErrIncompleteHistory},
		{"another space's row", 3, []client.HistoryEntry{foreign[0], history[1], history[2]}, nil},
	}
	for _, tc := range cases {
		m := client.New()
		err := m.OpenWithHistory(spaceID, wrapped, tc.epoch, tc.history, u)
		if err == nil {
			t.Fatalf("%s: opened", tc.name)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if m.IsOpen(spaceID) {
			t.Fatalf("%s: a failed open left the space open", tc.name)
		}
	}
}

// TestDecryptKeepsTheOpaqueError: a change no key on the ring opens fails with
// the library's one opaque decrypt error, as a single-key space did.
func TestDecryptKeepsTheOpaqueError(t *testing.T) {
	t.Parallel()
	_, r, u := dev(t)
	spaceID, wrapped, history, _ := rotated(t, r, 2)
	m := client.New()
	if err := m.OpenWithHistory(spaceID, wrapped, 2, history, u); err != nil {
		t.Fatal(err)
	}
	stranger, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encryption.EncryptChange(stranger, []byte("not this space"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Decrypt(spaceID, ct); !errors.Is(err, encryption.ErrDecrypt) {
		t.Fatalf("Decrypt = %v, want encryption.ErrDecrypt", err)
	}
}

// TestLoadAndVerifyCurrent: a current key held in the clear (a recovered one)
// unrolls the chain through Load; VerifyCurrent accepts it and refuses a stale
// key from an earlier epoch, which is what stops a stale recovery blob from
// being re-wrapped at the current epoch.
func TestLoadAndVerifyCurrent(t *testing.T) {
	t.Parallel()
	_, r, u := dev(t)
	spaceID, wrapped, history, cts := rotated(t, r, 2)
	current, err := u.Unwrap(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	m := client.New()
	if err := m.Load(spaceID, current, 2, history); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, err := m.Decrypt(spaceID, cts[0]); err != nil || string(got) != "written at epoch 0" {
		t.Fatalf("a loaded key did not unroll to epoch 0: %q %v", got, err)
	}
	if err := client.VerifyCurrent(current, 2, history); err != nil {
		t.Fatalf("VerifyCurrent(current) = %v", err)
	}
	keys, _ := m.Keys(spaceID)
	if err := client.VerifyCurrent(keys[1], 2, history); !errors.Is(err, client.ErrNotCurrentKey) {
		t.Fatalf("VerifyCurrent(stale) = %v, want ErrNotCurrentKey", err)
	}
	if err := client.VerifyCurrent(keys[2], 0, nil); err != nil {
		t.Fatalf("VerifyCurrent at epoch 0 = %v, want no check", err)
	}
}

// TestSealedPrevIsOpaqueAndTamperEvident: a history row does not open under any
// key but its epoch's, and a flipped byte anywhere is refused.
func TestSealedPrevIsOpaqueAndTamperEvident(t *testing.T) {
	t.Parallel()
	next, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	prev, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	row, err := client.SealPrevious(next, prev)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.OpenPrevious(next, row)
	if err != nil {
		t.Fatal(err)
	}
	probe, _ := encryption.EncryptChange(prev, []byte("probe"))
	if _, err := encryption.DecryptChange(got, probe); err != nil {
		t.Fatal("OpenPrevious did not return the previous key")
	}
	if _, err := client.OpenPrevious(prev, row); err == nil {
		t.Fatal("a history row opened under the wrong key")
	}
	for _, i := range []int{0, 1, 40, len(row) - 1} {
		bad := append([]byte(nil), row...)
		bad[i] ^= 0x01
		if _, err := client.OpenPrevious(next, bad); err == nil {
			t.Fatalf("a row with byte %d flipped opened", i)
		}
	}
}
