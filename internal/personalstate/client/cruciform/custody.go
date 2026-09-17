package cruciform

// Custody is the cruciform-offload backend as a personalstate/client.Custody: the
// wake+relay [Unwrapper] plus the RecipientID a space key is wrapped to. Unlike
// the software/yubikey/tpm backends — whose RecipientID is their own key's public
// point — the offload backend's wrap target is the PHONE's encryption key, which
// this desktop never holds a private half of. It comes from the pairing config
// pinned at pairing time, so opening a space looks up the phone's sealed copy and
// unwraps it via the phone.
type Custody struct {
	*Unwrapper
	recipientID string
}

// RecipientID reports the phone's "x25519:<hex>" wrap target — the copy of a
// space key the offload backend opens.
func (c *Custody) RecipientID() string { return c.recipientID }

// NewCustody builds the offload custody from a completed pairing and a transport.
// The transport carries the wake+relay round-trip to the paired phone (a
// [RelayTransport] in production; a fake in tests). The pairing config supplies
// the desktop transport key, the phone's device key, and the phone's encryption
// key (the RecipientID).
func NewCustody(cfg *PairConfig, transport Transport, opts ...Option) (*Custody, error) {
	u, err := New(transport, cfg.TransportKey, cfg.PhonePub, opts...)
	if err != nil {
		return nil, err
	}
	return &Custody{Unwrapper: u, recipientID: cfg.RecipientID()}, nil
}
