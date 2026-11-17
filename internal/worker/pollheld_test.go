package worker

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
)

// A transfer the client added stopped by its own default (#882) — Transmission
// with start-added-torrents off — is started by the poll while its want is in
// flight on it, and left alone once the want is not: a want Heyarr failed or
// moved on from is not waiting on that transfer.
func TestThePollStartsAHeldTransferOnlyWhileItsWantIsInFlight(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		failed  bool
		started bool
	}{
		{"the want is queued on it", false, true},
		{"the want was failed back to idle", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newPollHarness(t)
			h.grabAfterSearch(t, "Arrival.2016.2160p.mkv", []byte("the bytes of a film"))
			id := h.transferID(t)
			if tc.failed {
				if _, err := h.cat.AdvanceAcquisition(t.Context(), h.want,
					acquisition.TransitionFail, "failed elsewhere"); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.client.Hold(id); err != nil {
				t.Fatal(err)
			}

			h.poll(t)

			transfers, err := h.client.Transfers(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(transfers) != 1 {
				t.Fatalf("setup: %d transfers, want 1", len(transfers))
			}
			if started := !transfers[0].Held; started != tc.started {
				t.Fatalf("started = %v, want %v", started, tc.started)
			}
			if !tc.failed {
				if got := h.state(t).Phase; got != acquisition.PhaseQueued {
					t.Fatalf("phase = %s: starting a transfer moves no phase; its bytes do", got)
				}
			}
		})
	}
}
