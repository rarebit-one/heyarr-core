package useridentity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvDir overrides where the user identity store lives.
//
// It is read here and nowhere near config.Load, for the same reason
// device.EnvDir is: this directory is deliberately NOT part of the server's
// configuration. An identity directory that could be set in heyarr.yaml would
// sooner or later be set to the data directory by someone tidying paths, and
// that is the one value it must never take (ADR-0032).
const EnvDir = "HEYARR_IDENTITY_DIR"

// DefaultDir is where this machine keeps its user identity: the person's config
// directory, under `heyarr/identity` — a sibling of `heyarr/device`.
//
// os.UserConfigDir is a per-user location owned by the person at the keyboard,
// which is the property that matters. The server's data_dir is owned by the
// service account and readable by whoever operates the host; a private key
// there would be inside the blast radius the key exists to stay out of.
func DefaultDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(EnvDir)); dir != "" {
		return filepath.Clean(dir), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("useridentity: locating your configuration directory (set %s to choose one): %w",
			EnvDir, err)
	}
	return filepath.Join(base, "heyarr", "identity"), nil
}
