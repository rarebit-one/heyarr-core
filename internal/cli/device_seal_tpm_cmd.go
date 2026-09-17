package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/tpm"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
)

// newDeviceSealTPMCommand builds `heyarr device seal-tpm`: it seals THIS device's
// X25519 encryption key to the local TPM under a PCR+PIN policy (ADR-0098), so the
// vault's `tpm` custody backend can open spaces without an on-disk key. It seals
// the EXISTING device key, so its public point — the wrap target the controller
// already holds — is unchanged and every space already wrapped for this device
// still opens.
func newDeviceSealTPMCommand(_ Options, configPath, dir *string) *cobra.Command {
	var (
		outPath   string
		pcrArgs   []int
		tpmDevice string
	)
	cmd := &cobra.Command{
		Use:   "seal-tpm",
		Short: "Seal this device's encryption key to the TPM for hardware-gated custody (ADR-0098)",
		Long: `Seal this device's X25519 encryption key to the local TPM, so the vault's
` + "`tpm`" + ` custody backend opens spaces by unsealing after a TPM gate — a PCR
policy (the boot state) AND a PIN — rather than reading a key off disk.

It seals the EXISTING device key, so the public point stays the same and every
space already wrapped for this device keeps opening. The seed is written into a
TPM sealed object; the on-disk blob holds only the public point and ciphertext
the TPM alone can open.

The PIN is the sealed object's auth value, read from vault.tpm.pin_file or the
` + VaultTPMPINEnvVar + ` environment variable — the same source the backend
presents at unseal, so provisioning and opening agree. Requires a TPM 2.0
(Linux); the household Framework laptops (fTPM/PTT) are the target.

After sealing, select it with vault.unwrapper: tpm and confirm a space opens;
then you may remove the plaintext device key to complete the hardening — your
paper recovery secret still restores access if the TPM is ever lost or reset.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(derefStr(configPath))
			if err != nil {
				return err
			}
			ds, err := openDeviceStore(*dir)
			if err != nil {
				return err
			}
			priv, err := ds.LoadEncryptionKey()
			if err != nil {
				return fmt.Errorf("loading this device's encryption key (run `heyarr device generate` first): %w", err)
			}
			pin, err := pinFromFileOrEnv(cfg.Vault.TPM.PINFile, VaultTPMPINEnvVar, "vault.tpm.pin_file")()
			if err != nil {
				return err
			}
			out := outPath
			if out == "" {
				out = cfg.Vault.TPM.SealedKeyFile
			}
			if out == "" {
				return fmt.Errorf("no output path for the sealed key — pass --out or set vault.tpm.sealed_key_file")
			}
			pcrs, err := toUintPCRs(pcrArgs)
			if err != nil {
				return err
			}
			dev := tpmDevice
			if dev == "" {
				dev = cfg.Vault.TPM.Device
			}
			return sealDeviceKeyToTPM(cmd.OutOrStdout(), priv.Bytes(), pin, pcrs, tpm.OpenDevice(dev), out, ds.EncryptionKeyPath())
		},
	}
	cmd.Flags().StringVar(&outPath, "out", "", "where to write the sealed-key blob (default: vault.tpm.sealed_key_file)")
	cmd.Flags().IntSliceVar(&pcrArgs, "pcr", []int{7}, "PCR indices to bind the policy to (default: 7, the UEFI Secure Boot state)")
	cmd.Flags().StringVar(&tpmDevice, "tpm-device", "", "TPM device to open (default: /dev/tpmrm0, or vault.tpm.device)")
	return cmd
}

// sealDeviceKeyToTPM opens the TPM, seals the seed under the PCR+PIN policy,
// writes the blob, and prints what was done and how to finish. The opener is a
// parameter so the seal path is exercised against the reference simulator in
// tests; production passes tpm.OpenDevice.
func sealDeviceKeyToTPM(w io.Writer, seed []byte, pin string, pcrs []uint, opener tpm.Opener, outPath, seedFile string) error {
	conn, err := opener()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	blob, err := tpm.Seal(conn, seed, []byte(pin), tpm.PCRSelection(pcrs...))
	if err != nil {
		return err
	}
	if err := blob.WriteFile(outPath); err != nil {
		return err
	}
	fmt.Fprintf(w, "Sealed this device's encryption key to the TPM.\n\n"+
		"  public: %s\n  blob:   %s\n  PCRs:   %v\n\n"+
		"Select it with `vault.unwrapper: tpm` and `vault.tpm.sealed_key_file: %s`,\n"+
		"then confirm a space opens. Once it does you may remove the plaintext seed\n"+
		"%s to complete the hardening — your paper recovery secret still restores\n"+
		"access if the TPM is ever lost or reset.\n",
		encryption.FormatPublicKey(blob.Public), outPath, pcrs, outPath, seedFile)
	return nil
}

// toUintPCRs converts the --pcr flag ints to the uints PCRSelection wants,
// refusing a negative or out-of-range index.
func toUintPCRs(in []int) ([]uint, error) {
	out := make([]uint, 0, len(in))
	for _, p := range in {
		if p < 0 || p > 23 {
			return nil, fmt.Errorf("PCR index %d is out of range (0-23)", p)
		}
		out = append(out, uint(p))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one PCR is required to bind the policy")
	}
	return out, nil
}

// derefStr reads a *string, treating nil as empty — for the configPath threaded
// as a pointer through the command tree.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
