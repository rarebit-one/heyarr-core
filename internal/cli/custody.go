package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/yubikey"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/custody"
)

// VaultYubiKeyPINEnvVar is where the YubiKey custody backend reads the card User
// PIN when no pin file is configured — an environment variable, never an argument
// (a secret on the command line is in shell history and every user's `ps`).
const VaultYubiKeyPINEnvVar = "HEYARR_VAULT_YUBIKEY_PIN" // #nosec G101 -- the name of a variable, not a credential

// selectCustody builds this device's configured space-key custody backend
// (ADR-0098) — software by default, or the one named by vault.unwrapper. It is
// the single place the CLI's device-side paths (space/vault/device read, create,
// rotate) turn a config selection into a client.Custody, so create wraps to and
// open unwraps with the same key.
func selectCustody(configPath *string, deviceDir string) (client.Custody, error) {
	path := ""
	if configPath != nil {
		path = *configPath
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return custody.Select(custody.Options{
		Backend:       cfg.Vault.Unwrapper,
		DeviceDir:     deviceDir,
		YubiKeySocket: cfg.Vault.YubiKey.Socket,
		YubiKeyPIN:    yubikeyPINFunc(cfg.Vault.YubiKey.PINFile),
	})
}

// yubikeyPINFunc yields the card User PIN from the configured pin file, or from
// VaultYubiKeyPINEnvVar. It is read lazily (only when the backend actually
// deciphers on-card) and never persisted.
func yubikeyPINFunc(pinFile string) yubikey.PINFunc {
	return func() (string, error) {
		if pinFile != "" {
			raw, err := os.ReadFile(filepath.Clean(pinFile))
			if err != nil {
				return "", fmt.Errorf("reading the yubikey PIN file %s: %w", pinFile, err)
			}
			return strings.TrimSpace(string(raw)), nil
		}
		if v := os.Getenv(VaultYubiKeyPINEnvVar); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("no yubikey PIN — set %s or vault.yubikey.pin_file", VaultYubiKeyPINEnvVar)
	}
}
