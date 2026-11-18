package mcp_test

import (
	"testing"
	"time"
)

// get_acquisition_status reports a transfer held stopped at its client once
// the stop has outlasted the grace, with its reason, and not before (#883). A
// stop is not trouble: the want stays queued on the transfer.
func TestAcquisitionStatusShowsAStoppedTransferAfterTheGrace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		reason  string
		ago     time.Duration
		visible bool
	}{
		{"held for an hour", "held", time.Hour, true},
		{"paused for an hour", "after_run", time.Hour, true},
		{"stopped a minute ago", "after_run", time.Minute, false},
		{"running", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, false)
			id := h.wantOne("")
			// The harness's clock, which the status reads as now.
			now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
			stamp := now.Format(time.RFC3339Nano)
			var since any
			if tc.reason != "" {
				since = now.Add(-tc.ago).Format(time.RFC3339Nano)
			}
			h.exec(`UPDATE acquisition_state SET phase = 'queued' WHERE desired_item_id = ?`, id)
			h.exec(`INSERT INTO acquisitions
				(id, desired_item_id, provider, external_id, external_name, stopped, stopped_since,
				 created_at, updated_at, last_seen_at)
				VALUES ('01990000-0000-7000-8000-0000000000a9', ?, 'client-a', 'abc', 'Arrival.mkv', ?, ?, ?, ?, ?)`,
				id, tc.reason, since, stamp, stamp, stamp)

			var out struct {
				Phase    string `json:"phase"`
				Transfer struct {
					Trouble string `json:"trouble"`
					Stopped *struct {
						Reason string `json:"reason"`
						Since  string `json:"since"`
						Detail string `json:"detail"`
					} `json:"stopped"`
				} `json:"transfer"`
			}
			h.call("", "get_acquisition_status", `{"desired_item_id":"`+id+`"}`).structured(t, &out)
			if out.Phase != "queued" {
				t.Errorf("phase = %s, want queued: a stop must not fail the want", out.Phase)
			}
			if out.Transfer.Trouble != "" {
				t.Errorf("a stop was reported as trouble: %q", out.Transfer.Trouble)
			}
			switch {
			case !tc.visible && out.Transfer.Stopped != nil:
				t.Errorf("stopped = %+v, want it hidden", out.Transfer.Stopped)
			case tc.visible && out.Transfer.Stopped == nil:
				t.Fatal("stopped is absent, want it reported")
			case tc.visible:
				if out.Transfer.Stopped.Reason != tc.reason || out.Transfer.Stopped.Since == "" ||
					out.Transfer.Stopped.Detail == "" {
					t.Errorf("stopped = %+v, want reason %s with a since and a detail", out.Transfer.Stopped, tc.reason)
				}
			}
		})
	}
}
