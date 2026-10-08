package cli

import (
	"context"
	"crypto/ecdh"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/encryption"

	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/recipientkey"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

const (
	refSpaceWrapped   = "01926a00-0000-7000-8000-0000000000a1"
	refSpaceUnwrapped = "01926a00-0000-7000-8000-0000000000a2"
	refSpaceUnknown   = "01926a00-0000-7000-8000-0000000000a3"
	refObject         = "4f0e2c1a-9b8d-4c7e-a6f5-0123456789ab"
)

func wantExit(t *testing.T, what string, err error, code int) {
	t.Helper()
	var coded *exitError
	if !errors.As(err, &coded) || coded.ExitCode() != code {
		t.Errorf("%s: got %v, want exit %d", what, err, code)
	}
	if got := exitCode(err); err != nil && got != code {
		t.Errorf("%s: Main would exit %d, want %d", what, got, code)
	}
}

// get-ref classifies every way an executor's read can fail into the exit
// codes a runner maps without parsing prose: a space it cannot see (an
// ungranted one answers 404, exactly as an unknown one), one it cannot
// decrypt, an absent object, and a sealed key that will not open.
func TestVaultGetRefExitCodes(t *testing.T) {
	h := newAPIHarness(t)
	dir := t.TempDir()
	pinFile := filepath.Join(dir, "pin")
	if err := os.WriteFile(pinFile, []byte("pin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(dir, "recipient.sealed")
	key, err := recipientkey.Init(sealed, recipientkey.FilePIN(pinFile))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	raw, err := hex.DecodeString(strings.TrimPrefix(key.Recipient, "x25519:"))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	other, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	for id, to := range map[string]*ecdh.PublicKey{refSpaceWrapped: pub, refSpaceUnwrapped: other.PublicKey()} {
		if _, err := h.spaces.PutSpace(ctx, id, spaces.KindFamily); err != nil {
			t.Fatal(err)
		}
		sk, err := encryption.NewSpaceKey()
		if err != nil {
			t.Fatal(err)
		}
		wrapped, err := encryption.Seal(sk, to)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.spaces.PutWrappedKey(ctx, id, encryption.FormatPublicKey(to.Bytes()), wrapped, 0); err != nil {
			t.Fatal(err)
		}
	}

	exec := func(space string, extra ...string) (string, string, error) {
		args := append([]string{"vault", "get-ref", "hv1:" + space + "/" + refObject, "--sealed-key", sealed}, extra...)
		return h.run(args...)
	}
	_, _, err = exec(refSpaceUnknown, "--pin-file", pinFile)
	wantExit(t, "a space this credential cannot see", err, ExitVaultForbidden)
	_, _, err = exec(refSpaceUnwrapped, "--pin-file", pinFile)
	wantExit(t, "a space with no copy of its key for this recipient", err, ExitVaultUnwrap)
	stdout, stderr, err := exec(refSpaceWrapped, "--pin-file", pinFile)
	wantExit(t, "a ref naming no object", err, ExitVaultAbsent)
	if stdout != "" || stderr != "" {
		t.Errorf("a refusal wrote output: stdout %q stderr %q", stdout, stderr)
	}

	wrong := filepath.Join(dir, "wrong")
	if err := os.WriteFile(wrong, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = exec(refSpaceWrapped, "--pin-file", wrong)
	wantExit(t, "a wrong PIN", err, ExitVaultCustody)
	_, _, err = h.run("vault", "get-ref", "hv1:"+refSpaceWrapped+"/"+refObject,
		"--sealed-key", filepath.Join(dir, "absent.sealed"), "--pin-file", pinFile)
	wantExit(t, "no sealed key file", err, ExitVaultCustody)

	// A collection ref, or no ref at all, is refused before anything is read.
	if _, _, err := h.run("vault", "get-ref", "hv1:"+refSpaceWrapped, "--sealed-key", sealed); err == nil {
		t.Error("get-ref accepted a collection ref")
	}
	if _, _, err := h.run("vault", "get-ref", "hv1:"+strings.ToUpper(refSpaceWrapped)+"/"+refObject); err == nil {
		t.Error("get-ref accepted a malformed ref")
	}
}

// After the space opened, a blob the node cannot serve is an absent object
// (exit 6), not a refused grant; a 403 still means access went away.
func TestClassifyOpenedRead(t *testing.T) {
	ref := "hv1:" + refSpaceWrapped + "/" + refObject
	wantExit(t, "a missing blob", classifyOpenedRead(ref, &apiclient.Error{Status: http.StatusNotFound}), ExitVaultAbsent)
	wantExit(t, "an absent path", classifyOpenedRead(ref, errVaultPathAbsent), ExitVaultAbsent)
	wantExit(t, "access withdrawn mid-read", classifyOpenedRead(ref, &apiclient.Error{Status: http.StatusForbidden}), ExitVaultForbidden)
}

// put-ref takes a versioned, typed envelope and never echoes a refused one.
func TestReadObjectEnvelope(t *testing.T) {
	t.Parallel()
	for in, ok := range map[string]bool{
		`{"v":1,"type":"answer","body":"x"}`: true,
		`{"v":2,"type":"answer"}`:            false,
		`{"type":"answer"}`:                  false,
		`{"v":1}`:                            false,
		`["v",1]`:                            false,
		`null`:                               false,
		`secret-canary not json`:             false,
	} {
		_, err := readObject(strings.NewReader(in), "-")
		if (err == nil) != ok {
			t.Errorf("readObject(%q): %v, want ok=%v", in, err, ok)
		}
		if err != nil && strings.Contains(err.Error(), "canary") {
			t.Errorf("a refusal echoed the object: %v", err)
		}
	}
	big := `{"v":1,"type":"t","body":"` + strings.Repeat("a", maxVaultObject) + `"}`
	if _, err := readObject(strings.NewReader(big), "-"); err == nil {
		t.Error("an object over the bound was accepted")
	}
}

// The plaintext goes to a new owner-only file and never over an existing one.
func TestWritePlaintextIsExclusive(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "obj.json")
	if err := writePlaintext(nil, path, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the plaintext file is %v, want 0600", fi.Mode().Perm())
	}
	if err := writePlaintext(nil, path, []byte(`{"x":1}`)); err == nil {
		t.Error("writePlaintext replaced an existing file")
	}
}
