package downloads

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Why a client holds a transfer stopped (#883): held when it never ran,
// after_run when it did, nothing for one running, complete or errored.
func TestTransmissionStopReason(t *testing.T) {
	cases := []struct {
		name string
		t    torrent
		want providers.StopReason
	}{
		{"running", torrent{Status: statusDownloading, PercentDone: 0.4}, ""},
		{"added stopped and never run", torrent{Status: statusStopped}, providers.StoppedHeld},
		{"paused after it ran", torrent{Status: statusStopped, PercentDone: 0.4, DownloadedEver: 400, ActivityDate: 1}, providers.StoppedAfterRun},
		{"stopped having fetched nothing but run for a while", torrent{Status: statusStopped, SecondsDownloading: 90}, providers.StoppedAfterRun},
		{"stopped and complete", torrent{Status: statusStopped, PercentDone: 1}, ""},
		{"stopped on an error", torrent{Status: statusStopped, Error: 3}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stopReason(tc.t); got != tc.want {
				t.Errorf("stopReason = %q, want %q", got, tc.want)
			}
			if got, want := held(tc.t), tc.want == providers.StoppedHeld; got != want {
				t.Errorf("held = %v, want %v: Held and a held stop reason must agree", got, want)
			}
		})
	}
}

func TestQBittorrentStopReason(t *testing.T) {
	cases := []struct {
		name string
		t    qbTorrent
		done bool
		want providers.StopReason
	}{
		{"downloading", qbTorrent{State: "downloading", Downloaded: 10}, false, ""},
		{"qBittorrent 4 paused before it ran", qbTorrent{State: "pausedDL"}, false, providers.StoppedHeld},
		{"qBittorrent 5 stopped after it ran", qbTorrent{State: "stoppedDL", Downloaded: 10}, false, providers.StoppedAfterRun},
		{"paused once complete", qbTorrent{State: "pausedUP", Downloaded: 10}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := qbStopReason(tc.t, tc.done); got != tc.want {
				t.Errorf("qbStopReason = %q, want %q", got, tc.want)
			}
		})
	}
}
