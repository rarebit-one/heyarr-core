package relay

import (
	"testing"

	vbrelay "github.com/rarebit-one/voidbind-go/relay"
)

// A sealed cert slot carrying the admitting op plus up to rp.MaxPresentedOps
// membership ops must fit: the legacy 4 KiB cap produced a 413 mid-pairing
// on a real phone (2026-09-03). The node's cap is the wire's stated bound.
func TestSlotCapFitsAMembershipBearingCert(t *testing.T) {
	if MaxMessageBytes != vbrelay.DefaultMaxMessageBytes {
		t.Fatalf("MaxMessageBytes = %d, want voidbind-go's %d", MaxMessageBytes, vbrelay.DefaultMaxMessageBytes)
	}
	if MaxMessageBytes < 32<<10 {
		t.Fatalf("MaxMessageBytes = %d is too small for an op-bearing cert slot", MaxMessageBytes)
	}
}
