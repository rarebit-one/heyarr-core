// Package recipientkey is the executor side of a service recipient (ADR-0104):
// the X25519 key an executor's host holds so an owner can wrap space keys for
// it without enrolling it as a member device.
//
// The key is born straight into a passphrase-sealed file (void-which-binds-go
// custody/sealedfile, its ADR-0021): its seed is drawn in RAM and sealed, and is
// never written anywhere in the clear. The passphrase (the "PIN") is never an
// argument or an environment variable. It comes from a systemd credential
// ($CREDENTIALS_DIRECTORY/<name>, delivered by LoadCredentialEncrypted=) or
// from an owner-only file, and either source refuses rather than falls back.
//
// Reading the public key or its fingerprint never asks for the PIN: both come
// from the sealed file's clear header. Only an unwrap opens the seal.
package recipientkey

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rarebit-one/void-which-binds-go/custody"
	"github.com/rarebit-one/void-which-binds-go/custody/sealedfile"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/servicerecipient"
)

// DefaultPINCredential is the systemd credential name the sealed file's PIN is
// delivered under (LoadCredentialEncrypted=heyarr-recipient-pin:…).
const DefaultPINCredential = "heyarr-recipient-pin"

// DefaultUnlockTTL is how long an open recipient key holds its unsealed seed: a
// single command's unwrap, not a session.
const DefaultUnlockTTL = 30 * time.Second

// maxPINLen bounds how much of a PIN source is read.
const maxPINLen = 4096

// ErrNoCredential is a PIN credential that systemd did not deliver: no
// $CREDENTIALS_DIRECTORY, or no file of that name in it.
var ErrNoCredential = errors.New("recipientkey: the recipient PIN credential is not available " +
	"(run under systemd with LoadCredentialEncrypted=, or name a PIN file)")

// ErrNoSealedFile is a sealed key path with no file at it.
var ErrNoSealedFile = errors.New("recipientkey: there is no sealed recipient key at that path " +
	"(create one with `heyarr recipient init`)")

// Key is a recipient key's public half, as its host shows it.
type Key struct {
	// Recipient is the X25519 public key, "x25519:<hex>": the value
	// `heyarr recipient add --pub` takes.
	Recipient string `json:"recipient"`
	// Fingerprint is the value `heyarr recipient add` asks the owner to type.
	Fingerprint string `json:"fingerprint"`
	// Path is the sealed file that holds the private half.
	Path string `json:"path"`
}

// CredentialPIN reads the PIN from the systemd credential name, as
// LoadCredentialEncrypted= delivers it: $CREDENTIALS_DIRECTORY/<name>. It
// refuses when the directory is unset or relative, or the name is not a plain
// file name; otherwise it reads the file as FilePIN does.
func CredentialPIN(name string) custody.PINFunc {
	return func() (string, error) {
		dir := os.Getenv("CREDENTIALS_DIRECTORY")
		if dir == "" {
			return "", ErrNoCredential
		}
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("recipientkey: $CREDENTIALS_DIRECTORY %q is not absolute", dir)
		}
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
			return "", fmt.Errorf("recipientkey: %q is not a credential name", name)
		}
		pin, err := readPIN(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrNoCredential, name)
		}
		return pin, err
	}
}

// FilePIN reads the PIN from a file. The file must be a regular file readable
// by its owner only, at most 4 KiB, and not empty. One trailing line break is
// dropped, so a PIN written with `echo` works; nothing else is trimmed.
func FilePIN(path string) custody.PINFunc {
	return func() (string, error) { return readPIN(path) }
}

// PIN picks the PIN source: the file when one is named, else the systemd
// credential (DefaultPINCredential when credential is empty).
func PIN(file, credential string) custody.PINFunc {
	if file != "" {
		return FilePIN(file)
	}
	if credential == "" {
		credential = DefaultPINCredential
	}
	return CredentialPIN(credential)
}

func readPIN(path string) (string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "", fmt.Errorf("recipientkey: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return "", fmt.Errorf("recipientkey: %w", err)
	case !fi.Mode().IsRegular():
		return "", fmt.Errorf("recipientkey: the PIN source %s is not a regular file", path)
	case fi.Mode().Perm()&0o077 != 0:
		return "", fmt.Errorf("recipientkey: the PIN source %s is accessible to group or others (%v)", path, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxPINLen+1))
	defer clear(b)
	if err != nil {
		return "", fmt.Errorf("recipientkey: reading the PIN source %s: %w", path, err)
	}
	if len(b) > maxPINLen {
		return "", fmt.Errorf("recipientkey: the PIN source %s is longer than %d bytes", path, maxPINLen)
	}
	pin := b
	if p, ok := bytes.CutSuffix(pin, []byte("\n")); ok {
		pin, _ = bytes.CutSuffix(p, []byte("\r"))
	}
	if len(pin) == 0 {
		return "", fmt.Errorf("recipientkey: the PIN source %s is empty", path)
	}
	return string(pin), nil
}

// Init draws a fresh X25519 key straight into a new sealed file at path, sealed
// under the PIN pin yields. It refuses to replace an existing file: a second
// init would orphan every wrap made for the first key.
func Init(path string, pin custody.PINFunc) (Key, error) {
	if path == "" {
		return Key{}, errors.New("recipientkey: a sealed key path is required")
	}
	if pin == nil {
		return Key{}, errors.New("recipientkey: a PIN source is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Key{}, fmt.Errorf("recipientkey: %w", err)
	}
	p := &sealedfile.Provisioner{Path: path, PIN: pin}
	keys, err := p.Provision(custody.Spec{X25519: true})
	if err != nil {
		if errors.Is(err, sealedfile.ErrExists) {
			return Key{}, fmt.Errorf("%w — refusing to replace it: every space wrapped for its key would become unreadable", err)
		}
		return Key{}, err
	}
	defer func() { _ = keys.Close() }()
	return describe(path, keys)
}

// Show reads a sealed file's public key and fingerprint from its clear header,
// without asking for the PIN.
func Show(path string) (Key, error) {
	keys, err := open(path, func() (string, error) {
		return "", errors.New("recipientkey: showing a key never opens its seal")
	})
	if err != nil {
		return Key{}, err
	}
	defer func() { _ = keys.Close() }()
	return describe(path, keys)
}

// Open unseals the recipient key at path for ttl (DefaultUnlockTTL when zero)
// and returns its holder, which unwraps space keys sealed for it. A missing
// file, a wrong PIN or an unavailable PIN source fails here, before anything
// is fetched. Call the returned close func when done: it zeroes the seed.
func Open(path string, pin custody.PINFunc, ttl time.Duration) (custody.Holder, func(), error) {
	if pin == nil {
		return nil, nil, errors.New("recipientkey: a PIN source is required")
	}
	keys, err := open(path, pin)
	if err != nil {
		return nil, nil, err
	}
	if ttl <= 0 {
		ttl = DefaultUnlockTTL
	}
	if err := keys.Unlock(ttl); err != nil {
		return nil, nil, fmt.Errorf("recipientkey: unsealing %s: %w", path, err)
	}
	h, err := keys.Holder()
	if err != nil {
		_ = keys.Close()
		return nil, nil, fmt.Errorf("recipientkey: %s holds no X25519 key: %w", path, err)
	}
	return h, func() { _ = keys.Close() }, nil
}

func open(path string, pin custody.PINFunc) (*sealedfile.Keys, error) {
	if path == "" {
		return nil, errors.New("recipientkey: a sealed key path is required")
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoSealedFile, path)
	}
	keys, err := sealedfile.Open(path, pin)
	if err != nil {
		return nil, fmt.Errorf("recipientkey: %s: %w", path, err)
	}
	return keys, nil
}

func describe(path string, keys custody.Keys) (Key, error) {
	h, err := keys.Holder()
	if err != nil {
		return Key{}, fmt.Errorf("recipientkey: %s holds no X25519 key: %w", path, err)
	}
	recipient := h.RecipientID()
	fp, err := servicerecipient.Fingerprint(recipient)
	if err != nil {
		return Key{}, err
	}
	return Key{Recipient: recipient, Fingerprint: fp, Path: path}, nil
}
