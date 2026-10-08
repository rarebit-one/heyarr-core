package client_test

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/encryption"
	"github.com/rarebit-one/void-which-binds-go/testvectors"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
)

// keyChainVector is the library's key-chain golden case (void-which-binds-go
// testvectors/key-chain): the sealed_prev wire format every client shares.
type keyChainVector struct {
	Keys  []string `json:"keys"`
	Links []struct {
		Epoch      int    `json:"epoch"`
		SealedPrev string `json:"sealed_prev"`
	} `json:"links"`
	Refuse []struct {
		Name            string `json:"name"`
		SealingKeyIndex int    `json:"sealing_key_index"`
		Blob            string `json:"blob"`
	} `json:"refuse"`
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestUnrollReplaysTheLibraryKeyChainVector: the history rows of the
// "three-epochs" vector, unrolled from the epoch-2 key, yield keys 2, 1, 0 —
// so this client reads a chain any other client wrote — and every refuse blob
// is refused.
func TestUnrollReplaysTheLibraryKeyChainVector(t *testing.T) {
	t.Parallel()
	raw, err := testvectors.KeyChain("three-epochs")
	if err != nil {
		t.Fatal(err)
	}
	var v keyChainVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	keys := make([]encryption.SpaceKey, len(v.Keys))
	for i, k := range v.Keys {
		if keys[i], err = encryption.SpaceKeyFromBytes(mustHex(t, k)); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 3 || len(v.Links) != 2 {
		t.Fatalf("vector has %d keys and %d links, want 3 and 2", len(keys), len(v.Links))
	}
	history := make([]client.HistoryEntry, 0, len(v.Links))
	for _, l := range v.Links {
		history = append(history, client.HistoryEntry{Epoch: l.Epoch, SealedPrev: mustHex(t, l.SealedPrev)})
	}

	got, err := client.Unroll(keys[2], 2, history)
	if err != nil {
		t.Fatalf("Unroll: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Unroll gave %d keys, want 3", len(got))
	}
	for i, want := range []int{2, 1, 0} {
		probe, err := encryption.EncryptChange(keys[want], []byte("probe"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := encryption.DecryptChange(got[i], probe); err != nil {
			t.Fatalf("Unroll()[%d] is not keys[%d]", i, want)
		}
	}
	if err := client.VerifyCurrent(keys[2], 2, history); err != nil {
		t.Fatalf("VerifyCurrent(keys[2]) = %v", err)
	}

	for _, r := range v.Refuse {
		if _, err := client.OpenPrevious(keys[r.SealingKeyIndex], mustHex(t, r.Blob)); err == nil {
			t.Errorf("refuse case %q opened", r.Name)
		}
	}
}
