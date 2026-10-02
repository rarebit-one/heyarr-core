package cli

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"

	"github.com/rarebit-one/void-which-binds-go/recovery"
)

// A rewrap stage is authenticated with an HMAC-SHA256 tag in its STAGE-MAC
// file, keyed from the gen2 recovery secret (#692). SHA256SUMS is unkeyed:
// anyone who could write to the stage between --stage and --upload could
// replace a space's wraps and recovery.blob with wraps of a key they know (the
// recipients are public keys) and recompute the sums. For a space with no
// content the controller then has nothing to catch the forged key against. The
// tag can only be made with the gen2 secret, so --prove and --upload refuse
// any stage that was not written, byte for byte, by `--stage` with it.
//
// Key: HKDF-SHA256 with the secret's entropy as the input key material, an
// empty salt, and rewrapStageMACLabel as the info, 32 bytes. This mirrors how
// void-which-binds-go's recovery package derives its own seeds from the same
// entropy (HKDF-SHA256, no salt, one label per key); the library exposes no
// derivation under a caller's label, so the stdlib HKDF is used directly. The
// entropy is already a uniformly random 256-bit secret, so a salt adds nothing;
// the label alone separates this key from every key the library derives. The
// key is bound to its context by what it authenticates: the manifest names the
// gen2 user and recovery key, the device recipient and the space set.
//
// Message: rewrapStageMACLabel again (so a tag cannot be confused with any
// other use of an HMAC under a key from this secret), then the number of files,
// then each covered file's name and contents in rewrapMACFiles order. Every
// field is prefixed with its length as an 8-byte big-endian integer, so no
// byte can move from one field to the next: two files cannot be concatenated,
// split or swapped without changing the message.
const (
	rewrapMACFile       = "STAGE-MAC"
	rewrapStageMACLabel = "heyarr/rewrap-stage-mac/v1"
)

// rewrapMACFiles is every other file of a stage, in the order the MAC covers
// them: everything --prove and --upload read from the stage directory.
var rewrapMACFiles = []string{rewrapManifestFile, rewrapBlobFile, rewrapSumsFile}

// errStageMAC is a stage whose tag does not verify under the gen2 secret.
var errStageMAC = errors.New("the stage's " + rewrapMACFile + " does not verify under this gen2 secret: " +
	"the stage was changed after --stage, or was staged with another gen2 secret; nothing was trusted from it")

// rewrapStageMACKey derives the stage MAC key from the gen2 recovery secret.
func rewrapStageMACKey(secret recovery.Secret) ([]byte, error) {
	entropy := secret.Entropy()
	if len(entropy) == 0 {
		return nil, errors.New("no gen2 secret to authenticate the stage with")
	}
	return hkdf.Key(sha256.New, entropy, nil, rewrapStageMACLabel, sha256.Size)
}

// rewrapStageMAC computes the tag over the covered files, which must hold
// exactly the names in rewrapMACFiles.
func rewrapStageMAC(secret recovery.Secret, files map[string][]byte) ([]byte, error) {
	if len(files) != len(rewrapMACFiles) {
		return nil, fmt.Errorf("the stage MAC covers exactly %v", rewrapMACFiles)
	}
	key, err := rewrapStageMACKey(secret)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	writeMACField(mac, []byte(rewrapStageMACLabel))
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(rewrapMACFiles)))
	_, _ = mac.Write(n[:])
	for _, name := range rewrapMACFiles {
		data, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("the stage MAC covers exactly %v", rewrapMACFiles)
		}
		writeMACField(mac, []byte(name))
		writeMACField(mac, data)
	}
	return mac.Sum(nil), nil
}

func writeMACField(h hash.Hash, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(b)
}

// formatStageMAC is the STAGE-MAC file: the tag in lowercase hex, one line.
func formatStageMAC(tag []byte) []byte {
	return []byte(hex.EncodeToString(tag) + "\n")
}

// verifyStageMAC checks the STAGE-MAC file against the covered files in
// constant time. A missing, malformed or wrong tag is errStageMAC.
func verifyStageMAC(secret recovery.Secret, macFile []byte, files map[string][]byte) error {
	line, ok := strings.CutSuffix(string(macFile), "\n")
	if !ok || len(line) != 2*sha256.Size || strings.ToLower(line) != line {
		return fmt.Errorf("%s: malformed, want one line of %d lowercase hex digits: %w", rewrapMACFile, 2*sha256.Size, errStageMAC)
	}
	got, err := hex.DecodeString(line)
	if err != nil {
		return fmt.Errorf("%s: %w", rewrapMACFile, errStageMAC)
	}
	want, err := rewrapStageMAC(secret, files)
	if err != nil {
		return err
	}
	if !hmac.Equal(got, want) {
		return errStageMAC
	}
	return nil
}
