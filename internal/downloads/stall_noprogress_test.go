package downloads

import (
	"strings"
	"testing"
	"time"
)

// The no-progress rule (#811): an incomplete transfer the client is actively
// downloading that has fetched nothing for longer than its window is reported
// as no_seeders, so the stuck-grab path blocks the release and re-searches.
// Every other shape stays healthy.
func TestTransmissionReportsADeadSwarmAfterTheWindow(t *testing.T) {
	c, err := New(Options{Name: "t", Endpoint: "http://transmission.invalid:9091", Label: "heyarr", Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	hoursAgo := func(h int) int64 { return fixedNow.Add(-time.Duration(h) * time.Hour).Unix() }
	dead := torrent{
		HashString: strings.Repeat("a", 40), Status: statusDownloading, PercentDone: 0,
		AddedDate: hoursAgo(7), PeersConnected: 24, MetadataPercentComplete: 1,
		TrackerStats: []trackerStat{{HasAnnounced: true, LastAnnounceSucceeded: true, LastAnnounceResult: "Success", SeederCount: 0}},
	}
	if got := c.toTransfer(dead).Error; !strings.HasPrefix(got, string(TroubleNoSeeders)+": no piece data for 7h") ||
		!strings.Contains(got, "24 peers connected") || !strings.Contains(got, "trackers report 0 seeders") {
		t.Fatalf("a 7h dead swarm reported %q, want a no_seeders trouble naming the evidence", got)
	}

	cases := map[string]torrent{}
	recent := dead
	recent.ActivityDate = hoursAgo(1) // fetched an hour ago: a thin but live swarm
	cases["recent activity"] = recent
	young := dead
	young.AddedDate = hoursAgo(2) // added two hours ago: still inside the window
	cases["inside the window"] = young
	queued := dead
	queued.Status = 3 // waiting in the client's queue, not fetching by choice
	cases["queued"] = queued
	stopped := dead
	stopped.Status = statusStopped
	cases["stopped"] = stopped
	unknown := dead
	unknown.AddedDate, unknown.ActivityDate = 0, 0 // a client (or fixture) that does not say
	cases["no dates"] = unknown
	complete := dead
	complete.PercentDone = 1
	complete.Status = statusSeeding
	cases["complete"] = complete
	for name, tor := range cases {
		if got := c.toTransfer(tor).Error; got != "" {
			t.Errorf("%s: reported %q, want healthy", name, got)
		}
	}

	off, err := New(Options{Name: "t", Endpoint: "http://transmission.invalid:9091", Label: "heyarr", Now: func() time.Time { return fixedNow }, StallAfter: -1})
	if err != nil {
		t.Fatal(err)
	}
	if got := off.toTransfer(dead).Error; got != "" {
		t.Errorf("stall_after \"0\" (off) still reported %q", got)
	}
	short, err := New(Options{Name: "t", Endpoint: "http://transmission.invalid:9091", Label: "heyarr", Now: func() time.Time { return fixedNow }, StallAfter: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got := short.toTransfer(young).Error; !strings.HasPrefix(got, string(TroubleNoSeeders)) {
		t.Errorf("a 1h window did not report a transfer idle for 2h: %q", got)
	}
}

func TestQBittorrentReportsADeadSwarmAfterTheWindow(t *testing.T) {
	c, err := NewQBittorrent(QBOptions{Name: "qb", Endpoint: "http://qb.invalid:8080", Label: "heyarr", Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	dead := qbTorrent{Hash: strings.Repeat("b", 40), State: "metaDL", AddedOn: fixedNow.Add(-8 * time.Hour).Unix()}
	if got := c.toTransfer(dead).Error; !strings.HasPrefix(got, string(TroubleNoSeeders)+": no piece data for 8h") {
		t.Fatalf("an 8h metaDL transfer reported %q, want no_seeders", got)
	}
	live := dead
	live.State = "stalledDL"
	live.LastActivity = fixedNow.Add(-30 * time.Minute).Unix()
	paused := dead
	paused.State = "pausedDL"
	for name, tor := range map[string]qbTorrent{"recent activity": live, "paused": paused} {
		if got := c.toTransfer(tor).Error; got != "" {
			t.Errorf("%s: reported %q, want healthy", name, got)
		}
	}
}
