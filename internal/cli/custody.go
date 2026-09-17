package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/cruciform"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/custody"
)

// The hardware backends read their PIN from an environment variable when no pin
// file is configured — never an argument (a secret on the command line is in
// shell history and every user's `ps`).
const (
	VaultYubiKeyPINEnvVar = "HEYARR_VAULT_YUBIKEY_PIN" // #nosec G101 -- the name of a variable, not a credential
	VaultTPMPINEnvVar     = "HEYARR_VAULT_TPM_PIN"     // #nosec G101 -- the name of a variable, not a credential
)

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
	// Resolve the device directory once, so the cruciform pairing file defaults to
	// the same directory the software backend loads its key from (custody.Select
	// with a non-empty DeviceDir skips its own resolution, so other backends are
	// unchanged).
	if deviceDir == "" {
		deviceDir, err = device.DefaultDir()
		if err != nil {
			return nil, err
		}
	}
	return custody.Select(custody.Options{
		Backend:          cfg.Vault.Unwrapper,
		DeviceDir:        deviceDir,
		YubiKeySocket:    cfg.Vault.YubiKey.Socket,
		YubiKeyPIN:       pinFromFileOrEnv(cfg.Vault.YubiKey.PINFile, VaultYubiKeyPINEnvVar, "vault.yubikey.pin_file"),
		TPMSealedKeyFile: cfg.Vault.TPM.SealedKeyFile,
		TPMDevice:        cfg.Vault.TPM.Device,
		TPMPIN:           pinFromFileOrEnv(cfg.Vault.TPM.PINFile, VaultTPMPINEnvVar, "vault.tpm.pin_file"),
		// The wake (the RP endpoint the desktop calls) is a follow-up; until it is
		// wired the offload opens only when the phone is already reachable, so the
		// Options' CruciformWake stays nil here.
		CruciformPairFile: cruciformPairFile(cfg.Vault.Cruciform.PairFile, deviceDir),
	})
}

// cruciformPairFile resolves the offload pairing config path: the configured
// override, or cruciform-pairing.json in the device directory — the default
// location `heyarr device pair-offload` writes.
func cruciformPairFile(configured, deviceDir string) string {
	if configured != "" {
		return configured
	}
	return filepath.Join(deviceDir, cruciform.PairConfigFileName)
}

// pinFromFileOrEnv yields a backend PIN from the configured pin file, or from the
// named environment variable. It is read lazily (only when the backend actually
// gates) and never persisted. The returned func matches both yubikey.PINFunc and
// tpm.PINFunc (both `func() (string, error)`).
func pinFromFileOrEnv(pinFile, envVar, cfgKey string) func() (string, error) {
	return func() (string, error) {
		if pinFile != "" {
			raw, err := os.ReadFile(filepath.Clean(pinFile))
			if err != nil {
				return "", fmt.Errorf("reading the PIN file %s: %w", pinFile, err)
			}
			return strings.TrimSpace(string(raw)), nil
		}
		if v := os.Getenv(envVar); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("no PIN — set %s or %s", envVar, cfgKey)
	}
}
