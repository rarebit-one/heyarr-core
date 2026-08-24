package render

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// SecretFile is where the signing secret lives inside the data directory.
//
// It sits beside the peer's Ed25519 key rather than in the database because it
// belongs to the node that serves bytes, not to the catalog: a peer restored
// from a controller backup should not inherit another node's signing key
// (ADR-0040 scopes a capability to the peer that minted it).
const SecretFile = "render_secret"

// EnsureSecret loads the signing secret, generating it on first use.
//
// Losing it is harmless — it invalidates outstanding capabilities, all of which
// expire within hours anyway, and the next mint uses the new one. So this is
// deliberately not something an operator has to back up or rotate, and there is
// no command to manage it. If it ever becomes something worth keeping, that is
// the signal that capabilities have grown into §77 grants and belong in a table.
func EnsureSecret(dataDir string) ([]byte, error) {
	if dataDir == "" {
		return nil, errors.New("render: a data directory is required")
	}
	path := filepath.Join(dataDir, SecretFile)

	secret, err := os.ReadFile(path) //nolint:gosec // a path this process composed
	switch {
	case err == nil:
		if len(secret) != SecretLen {
			// A truncated or overwritten secret is refused rather than padded
			// or regenerated. Regenerating would silently invalidate every
			// outstanding capability; the operator should know the file is
			// wrong, and deleting it is the documented fix.
			return nil, fmt.Errorf("render: %s is %d bytes, want %d — delete it to regenerate",
				path, len(secret), SecretLen)
		}
		return secret, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("render: reading the signing secret: %w", err)
	}

	secret, err = NewSecret()
	if err != nil {
		return nil, err
	}
	// 0600, and written through a temporary file so that a crash mid-write
	// leaves either the old secret or none — never a half-length one, which is
	// the case the length check above would then refuse to start against.
	tmp, err := os.CreateTemp(dataDir, SecretFile+".*")
	if err != nil {
		return nil, fmt.Errorf("render: creating the signing secret: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("render: securing the signing secret: %w", err)
	}
	if _, err := tmp.Write(secret); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("render: writing the signing secret: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("render: closing the signing secret: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, fmt.Errorf("render: installing the signing secret: %w", err)
	}
	return secret, nil
}
