package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rarebit-one/void-which-binds-go/device"
	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/cruciform"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/custody"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/recipientkey"
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
	return selectCustodyWith(configPath, deviceDir, custodyOverride{})
}

// custodyOverride is a per-invocation custody selection that wins over the
// config file: the vault commands' --unwrapper/--sealed-key/--pin-file, so an
// executor can name its sealed recipient key without a config of its own.
type custodyOverride struct {
	unwrapper string
	sealedKey string
	pinFile   string
}

func (o *custodyOverride) register(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&o.unwrapper, "unwrapper", "",
		"the custody backend that opens space keys (default: vault.unwrapper); "+
			"sealedfile is an executor's service-recipient key (ADR-0104)")
	cmd.PersistentFlags().StringVar(&o.sealedKey, "sealed-key", "",
		"the `file` holding the sealed recipient key from heyarr recipient init (implies --unwrapper sealedfile; default: vault.sealedfile.key_file)")
	cmd.PersistentFlags().StringVar(&o.pinFile, "pin-file", "",
		"an owner-only file holding the sealed key's PIN (default: vault.sealedfile.pin_file, else the systemd credential "+
			recipientkey.DefaultPINCredential+")")
}

func selectCustodyWith(configPath *string, deviceDir string, ov custodyOverride) (client.Custody, error) {
	path := ""
	if configPath != nil {
		path = *configPath
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if ov.sealedKey != "" && ov.unwrapper == "" {
		ov.unwrapper = custody.SealedFile
	}
	if ov.unwrapper != "" {
		cfg.Vault.Unwrapper = ov.unwrapper
	}
	if ov.sealedKey != "" {
		cfg.Vault.SealedFile.KeyFile = ov.sealedKey
	}
	if ov.pinFile != "" {
		cfg.Vault.SealedFile.PINFile = ov.pinFile
	}
	// The executor's key is not a device's: it needs no device directory, and
	// resolving one could fail on a host with no config directory at all.
	if cfg.Vault.Unwrapper == custody.SealedFile {
		return custody.Select(custody.Options{
			Backend:       custody.SealedFile,
			SealedKeyFile: cfg.Vault.SealedFile.KeyFile,
			SealedKeyPIN:  recipientkey.PIN(cfg.Vault.SealedFile.PINFile, cfg.Vault.SealedFile.PINCredential),
		})
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
	opts := custody.Options{
		Backend:           cfg.Vault.Unwrapper,
		DeviceDir:         deviceDir,
		YubiKeySocket:     cfg.Vault.YubiKey.Socket,
		YubiKeyPIN:        pinFromFileOrEnv(cfg.Vault.YubiKey.PINFile, VaultYubiKeyPINEnvVar, "vault.yubikey.pin_file"),
		TPMSealedKeyFile:  cfg.Vault.TPM.SealedKeyFile,
		TPMDevice:         cfg.Vault.TPM.Device,
		TPMPIN:            pinFromFileOrEnv(cfg.Vault.TPM.PINFile, VaultTPMPINEnvVar, "vault.tpm.pin_file"),
		CruciformPairFile: cruciformPairFile(cfg.Vault.Cruciform.PairFile, deviceDir),
	}
	// For the offload backend, wire the away-path wake so an unwrap can wake the
	// paired phone via the node (nil for the other backends, and nil when this
	// device is not enrolled — the offload then opens only when the phone is
	// already reachable).
	if opts.Backend == custody.Cruciform {
		wake, err := buildCruciformWake(cfg, deviceDir)
		if err != nil {
			return nil, err
		}
		opts.CruciformWake = wake
	}
	return custody.Select(opts)
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
