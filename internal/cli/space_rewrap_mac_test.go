package cli

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/recovery"
)

// macVectorFiles is the stage-MAC known-answer vector's input. The expected
// key and tag were computed independently of this package (Python's hmac and
// hashlib, RFC 5869 HKDF), so the test pins the construction, not this code's
// reading of it.
func macVectorFiles() map[string][]byte {
	return map[string][]byte{
		rewrapManifestFile: []byte("{\"format\":\"heyarr-rewrap-stage-v1\"}\n"),
		rewrapBlobFile:     []byte("\x00\x01blob"),
		rewrapSumsFile:     []byte("sums\n"),
	}
}

func macVectorSecret(t *testing.T) recovery.Secret {
	t.Helper()
	entropy := make([]byte, recovery.SecretEntropyBytes)
	for i := range entropy {
		entropy[i] = byte(i)
	}
	s, err := recovery.SecretFromEntropy(entropy)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestRewrapStageMACKnownAnswer pins the key derivation (HKDF-SHA256, empty
// salt, info heyarr/rewrap-stage-mac/v1) and the length-prefixed message.
func TestRewrapStageMACKnownAnswer(t *testing.T) {
	s := macVectorSecret(t)
	key, err := rewrapStageMACKey(s)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hex.EncodeToString(key), "6b4c97f2379f9d5a4db1353d402897fe47f2a6d7c973ce95f1c8bc9750ce3f47"; got != want {
		t.Fatalf("key = %s, want %s", got, want)
	}
	tag, err := rewrapStageMAC(s, macVectorFiles())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hex.EncodeToString(tag), "c1f78b37f75813a62dadc18d688238579b416cf913df0a2fde739f8d221777c3"; got != want {
		t.Fatalf("tag = %s, want %s", got, want)
	}
	if err := verifyStageMAC(s, formatStageMAC(tag), macVectorFiles()); err != nil {
		t.Fatalf("the vector's own tag: %v", err)
	}
}

// TestRewrapStageMACSeparatesFields: moving bytes across a file boundary, or
// swapping two files' contents, changes the tag, so files cannot be
// concatenated, split or swapped under one tag.
func TestRewrapStageMACSeparatesFields(t *testing.T) {
	s := macVectorSecret(t)
	base := macVectorFiles()
	tag, err := rewrapStageMAC(s, base)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]map[string][]byte{
		"a byte moved from the manifest to the blob": {
			rewrapManifestFile: base[rewrapManifestFile][:len(base[rewrapManifestFile])-1],
			rewrapBlobFile:     append([]byte{'\n'}, base[rewrapBlobFile]...),
			rewrapSumsFile:     base[rewrapSumsFile],
		},
		"the blob and the sums swapped": {
			rewrapManifestFile: base[rewrapManifestFile],
			rewrapBlobFile:     base[rewrapSumsFile],
			rewrapSumsFile:     base[rewrapBlobFile],
		},
		"everything in the manifest": {
			rewrapManifestFile: bytes.Join([][]byte{base[rewrapManifestFile], base[rewrapBlobFile], base[rewrapSumsFile]}, nil),
			rewrapBlobFile:     nil,
			rewrapSumsFile:     nil,
		},
	}
	for name, files := range cases {
		if err := verifyStageMAC(s, formatStageMAC(tag), files); !errors.Is(err, errStageMAC) {
			t.Errorf("%s: err = %v, want errStageMAC", name, err)
		}
	}
	other, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyStageMAC(other, formatStageMAC(tag), base); !errors.Is(err, errStageMAC) {
		t.Errorf("another secret: err = %v, want errStageMAC", err)
	}
	for _, bad := range []string{
		"", "00\n", hex.EncodeToString(tag), hex.EncodeToString(tag) + "\n\n",
		"  " + hex.EncodeToString(tag)[2:] + "\n",
	} {
		if err := verifyStageMAC(s, []byte(bad), base); !errors.Is(err, errStageMAC) {
			t.Errorf("STAGE-MAC %q: err = %v, want errStageMAC", bad, err)
		}
	}
	upper := bytes.ToUpper(formatStageMAC(tag))
	if err := verifyStageMAC(s, upper, base); !errors.Is(err, errStageMAC) {
		t.Errorf("uppercase STAGE-MAC: err = %v, want errStageMAC", err)
	}
}
