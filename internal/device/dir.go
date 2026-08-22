package device

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvDir overrides where the device store lives.
//
// It is read here and nowhere near config.Load: this directory is deliberately
// NOT part of the server's configuration. A device directory that could be set
// in heyarr.yaml would sooner or later be set to the data directory by someone
// tidying paths, and that is the one value it must never take.
const EnvDir = "HEYARR_DEVICE_DIR"

// DefaultDir is where this machine keeps its device key: the user's config
// directory, under `heyarr/device`.
//
// os.UserConfigDir is $XDG_CONFIG_HOME (or ~/.config) on Linux,
// ~/Library/Application Support on macOS and %AppData% on Windows — in every
// case a per-user location owned by the person at the keyboard, which is the
// property that matters. The server's data_dir is owned by the service account,
// backed up with the catalog and readable by whoever operates the host; a
// private key there would be inside the blast radius the key exists to stay out
// of.
func DefaultDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(EnvDir)); dir != "" {
		return filepath.Clean(dir), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("device: locating your configuration directory (set %s to choose one): %w",
			EnvDir, err)
	}
	return filepath.Join(base, "heyarr", "device"), nil
}
