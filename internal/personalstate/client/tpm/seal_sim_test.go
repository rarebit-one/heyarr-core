//go:build tpmsim

package tpm

// Seal is the legacy sealing path, kept for the simulator round trip only:
// `heyarr device seal-tpm`, its one production caller, is disabled until
// void-which-binds-go's custody/tpm lands (ADR-0021), and a gen2 key must not
// be sealed into this format.

import (
	"fmt"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
	"github.com/rarebit-one/void-which-binds-go/encryption"
)

// Seal seals an X25519 seed to the TPM under a PolicyPCR+PolicyAuthValue policy,
// returning the Blob to persist. The seed's public point is recorded on the Blob.
func Seal(tpm transport.TPM, seed, pin []byte, pcrSel tpm2.TPMLPCRSelection) (Blob, error) {
	priv, err := encryption.NewPrivateKey(seed)
	if err != nil {
		return Blob{}, fmt.Errorf("tpm: the seed is not a valid X25519 key: %w", err)
	}
	parent, cleanup, err := primarySRK(tpm)
	if err != nil {
		return Blob{}, err
	}
	defer cleanup()
	pol, err := policyDigest(tpm, pcrSel)
	if err != nil {
		return Blob{}, err
	}
	tmpl := tpm2.TPMTPublic{
		Type:    tpm2.TPMAlgKeyedHash,
		NameAlg: tpm2.TPMAlgSHA256,
		// Policy-only: unseal must satisfy the policy, which includes AuthValue.
		ObjectAttributes: tpm2.TPMAObject{FixedTPM: true, FixedParent: true},
		AuthPolicy:       tpm2.TPM2BDigest{Buffer: pol},
		Parameters: tpm2.NewTPMUPublicParms(tpm2.TPMAlgKeyedHash, &tpm2.TPMSKeyedHashParms{
			Scheme: tpm2.TPMTKeyedHashScheme{Scheme: tpm2.TPMAlgNull},
		}),
	}
	cr, err := (tpm2.Create{
		ParentHandle: parent,
		InSensitive: tpm2.TPM2BSensitiveCreate{Sensitive: &tpm2.TPMSSensitiveCreate{
			UserAuth: tpm2.TPM2BAuth{Buffer: pin},
			Data:     tpm2.NewTPMUSensitiveCreate(&tpm2.TPM2BSensitiveData{Buffer: seed}),
		}},
		InPublic: tpm2.New2B(tmpl),
	}).Execute(tpm)
	if err != nil {
		return Blob{}, fmt.Errorf("tpm: sealing the seed: %w", err)
	}
	return Blob{
		Public:     priv.PublicKey().Bytes(),
		PCRs:       pcrSel,
		SealedPub:  cr.OutPublic,
		SealedPriv: cr.OutPrivate,
	}, nil
}

// policyDigest computes the authPolicy the sealed object is bound to: a trial
// session running PolicyPCR then PolicyAuthValue.
func policyDigest(tpm transport.TPM, pcrSel tpm2.TPMLPCRSelection) ([]byte, error) {
	trial, cleanup, err := tpm2.PolicySession(tpm, tpm2.TPMAlgSHA256, 16, tpm2.Trial())
	if err != nil {
		return nil, fmt.Errorf("tpm: trial session: %w", err)
	}
	defer func() { _ = cleanup() }()
	if _, err := (tpm2.PolicyPCR{PolicySession: trial.Handle(), Pcrs: pcrSel}).Execute(tpm); err != nil {
		return nil, fmt.Errorf("tpm: trial PolicyPCR: %w", err)
	}
	if _, err := (tpm2.PolicyAuthValue{PolicySession: trial.Handle()}).Execute(tpm); err != nil {
		return nil, fmt.Errorf("tpm: trial PolicyAuthValue: %w", err)
	}
	dig, err := (tpm2.PolicyGetDigest{PolicySession: trial.Handle()}).Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("tpm: PolicyGetDigest: %w", err)
	}
	return dig.PolicyDigest.Buffer, nil
}
