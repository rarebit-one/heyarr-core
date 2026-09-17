package cruciform

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// pairConfigFileMode is 0600 — the pairing config holds the desktop's transport
// private key, so it is readable only by its owner, like the device key files it
// sits beside.
const pairConfigFileMode = 0o600

// SavePairConfig writes a completed pairing to path (0600), creating parent
// directories as needed. It overwrites any existing pairing there — re-pairing
// replaces the pin.
func SavePairConfig(path string, cfg *PairConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cruciform: creating the pairing directory: %w", err)
		}
	}
	if err := os.WriteFile(path, data, pairConfigFileMode); err != nil {
		return fmt.Errorf("cruciform: writing the pairing config: %w", err)
	}
	return nil
}

// LoadPairConfig reads and validates the pairing config at path. A missing file
// means this device is not paired for offload; callers surface that as "run
// `heyarr device pair-offload` first".
func LoadPairConfig(path string) (*PairConfig, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var cfg PairConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
