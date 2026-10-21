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

	// Each case is its own transfer: the clock is keyed by infohash.
	cases := map[string]torrent{}
	young := dead
	young.AddedDate = hoursAgo(2) // added two hours ago: still inside the window
	cases["inside the window"] = young
	partial := dead
	partial.PercentDone = 0.4 // holds data: first sight opens the window now
	cases["partial, first sight"] = partial
	queued := dead
	queued.Status = 3 // waiting in the client's queue, not fetching by choice
	cases["queued"] = queued
	stopped := dead
	stopped.Status = statusStopped
	cases["stopped"] = stopped
	unknown := dead
	unknown.AddedDate = 0 // a client (or fixture) that does not say
	cases["no dates"] = unknown
	complete := dead
	complete.PercentDone = 1
	complete.Status = statusSeeding
	cases["complete"] = complete
	i := 0
	for name, tor := range cases {
		i++
		tor.HashString = strings.Repeat(string(rune('b'+i)), 40)
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

// A partial transfer that keeps UPLOADING what it has while fetching nothing
// still stalls. Both clients' activity timestamps move on uploads, which is
// why the rule watches the fetched amount across polls instead.
func TestTransmissionAPartialTransferThatOnlyUploadsStalls(t *testing.T) {
	now := fixedNow
	c, err := New(Options{Name: "t", Endpoint: "http://transmission.invalid:9091", Label: "heyarr", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stuck := torrent{
		HashString: strings.Repeat("1", 40), Status: statusDownloading, PercentDone: 0.4,
		AddedDate: fixedNow.Add(-10 * time.Hour).Unix(),
	}
	moving := stuck
	moving.HashString = strings.Repeat("2", 40)

	for _, step := range []struct {
		at        time.Duration
		moved     float64
		wantStuck bool
	}{
		{0, 0.4, false},
		{5 * time.Hour, 0.5, false},
		{7 * time.Hour, 0.5, true}, // 7h since stuck last fetched; moving fetched 2h ago
	} {
		now = fixedNow.Add(step.at)
		moving.PercentDone = step.moved
		if got := c.toTransfer(stuck).Error; (got != "") != step.wantStuck {
			t.Errorf("+%s: the uploading-only transfer reported %q, want stalled=%v", step.at, got, step.wantStuck)
		}
		if got := c.toTransfer(moving).Error; got != "" {
			t.Errorf("+%s: a transfer still fetching reported %q", step.at, got)
		}
	}
}

func TestQBittorrentReportsADeadSwarmAfterTheWindow(t *testing.T) {
	now := fixedNow
	c, err := NewQBittorrent(QBOptions{Name: "qb", Endpoint: "http://qb.invalid:8080", Label: "heyarr", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	dead := qbTorrent{Hash: strings.Repeat("b", 40), State: "metaDL", AddedOn: fixedNow.Add(-8 * time.Hour).Unix()}
	if got := c.toTransfer(dead).Error; !strings.HasPrefix(got, string(TroubleNoSeeders)+": no piece data for 8h") {
		t.Fatalf("an 8h metaDL transfer reported %q, want no_seeders", got)
	}
	paused := dead
	paused.Hash = strings.Repeat("c", 40)
	paused.State = "pausedDL"
	if got := c.toTransfer(paused).Error; got != "" {
		t.Errorf("paused: reported %q, want healthy", got)
	}

	// Partial and uploading: healthy while it fetches, stalled once it stops.
	partial := qbTorrent{
		Hash: strings.Repeat("d", 40), State: "stalledDL", Completed: 400,
		AddedOn: fixedNow.Add(-10 * time.Hour).Unix(),
	}
	if got := c.toTransfer(partial).Error; got != "" {
		t.Errorf("partial at first sight reported %q, want healthy", got)
	}
	now = fixedNow.Add(3 * time.Hour)
	partial.Completed = 500
	if got := c.toTransfer(partial).Error; got != "" {
		t.Errorf("partial that fetched reported %q, want healthy", got)
	}
	now = fixedNow.Add(10 * time.Hour)
	if got := c.toTransfer(partial).Error; !strings.HasPrefix(got, string(TroubleNoSeeders)+": no piece data for 7h") {
		t.Errorf("partial idle for 7h reported %q, want no_seeders", got)
	}
}

func TestProgressClockForgetsTransfersThatLeave(t *testing.T) {
	p := newProgressClock()
	p.lastProgress("gone", 5, time.Time{}, fixedNow)
	p.lastProgress("here", 5, time.Time{}, fixedNow.Add(forgetAfter+2*time.Hour))
	if _, ok := p.marks["gone"]; ok {
		t.Error("a transfer unseen for longer than forgetAfter is still remembered")
	}
	if got := (*progressClock)(nil).lastProgress("x", 0, fixedNow, fixedNow); !got.IsZero() {
		t.Errorf("a nil clock returned %v, want the zero time", got)
	}
}
