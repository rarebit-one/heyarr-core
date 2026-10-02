package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// errSealTPMDisabled is what `heyarr device seal-tpm` says now. The command
// stays, so a doc or script that still runs it gets this rather than "unknown
// command".
var errSealTPMDisabled = errors.New("`heyarr device seal-tpm` is disabled: it sealed a device key into heyarr's " +
	"legacy TPM blob, which void-which-binds-go's custody/tpm backend will never read, and every device key is now " +
	"gen2 (void-which-binds-go ADR-0021, ADR-0022). TPM custody returns through that backend after the C2 cutover; " +
	"until then use `heyarr device generate --custody sealedfile`, or keep a software device")

// newDeviceSealTPMCommand builds `heyarr device seal-tpm`, disabled until
// void-which-binds-go's custody/tpm lands (ADR-0021). It sealed THIS device's
// existing X25519 seed to the local TPM in heyarr's legacy blob format; a gen2
// key must not be sealed that way, because the library never reads that format.
// Its flags are kept so an old invocation parses and reaches the explanation.
func newDeviceSealTPMCommand(_ Options, _, _ *string) *cobra.Command {
	var (
		outPath   string
		pcrArgs   []int
		tpmDevice string
	)
	cmd := &cobra.Command{
		Use:   "seal-tpm",
		Short: "Disabled: TPM custody returns with void-which-binds-go's custody/tpm (ADR-0021)",
		Long: `Disabled until void-which-binds-go's custody/tpm backend lands, after the C2
cutover (void-which-binds-go ADR-0021).

This command sealed a device's existing X25519 seed into heyarr's legacy TPM
blob for the vault's ` + "`tpm`" + ` backend. Every device key is now gen2
(ADR-0022), and the library's TPM backend will never read the legacy blob, so a
gen2 key sealed with it would be stranded in a format nothing supports. It now
refuses, whatever its flags.

Until custody/tpm lands, a machine with no other gate holds its device keys in a
passphrase-sealed file (` + "`heyarr device generate --custody sealedfile`" + `),
and an unattended service device keeps a software device.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(*cobra.Command, []string) error { return errSealTPMDisabled },
	}
	cmd.Flags().StringVar(&outPath, "out", "", "ignored: the command is disabled")
	cmd.Flags().IntSliceVar(&pcrArgs, "pcr", nil, "ignored: the command is disabled")
	cmd.Flags().StringVar(&tpmDevice, "tpm-device", "", "ignored: the command is disabled")
	return cmd
}
