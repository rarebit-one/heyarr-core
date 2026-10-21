package downloads

import (
	"strings"
	"testing"
	"time"
)

// The no-progress rule (#811): an incomplete transfer the client is actively
// downloading that has fetched nothing for longer than its window is reported
// as no_seeders, so the stuck-grab path blocks the release and re-searches.
// The window opens at the first poll that sees it active. Every other shape
// stays healthy.
func TestTransmissionReportsADeadSwarmAfterTheWindow(t *testing.T) {
	now := fixedNow
	c, err := New(Options{Name: "t", Endpoint: "http://transmission.invalid:9091", Label: "heyarr", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	dead := torrent{
		HashString: strings.Repeat("a", 40), Status: statusDownloading, PercentDone: 0,
		AddedDate: fixedNow.Add(-30 * time.Hour).Unix(), PeersConnected: 24, MetadataPercentComplete: 1,
		TrackerStats: []trackerStat{{HasAnnounced: true, LastAnnounceSucceeded: true, LastAnnounceResult: "Success", SeederCount: 0}},
	}
	if got := c.toTransfer(dead).Error; got != "" {
		t.Fatalf("first active sight reported %q: the window opens now, not at the add date", got)
	}
	now = fixedNow.Add(7 * time.Hour)
	if got := c.toTransfer(dead).Error; !strings.HasPrefix(got, string(TroubleNoSeeders)+": no piece data for 7h") ||
		!strings.Contains(got, "24 peers connected") || !strings.Contains(got, "trackers report 0 seeders") {
		t.Fatalf("a swarm dead for 7h reported %q, want a no_seeders trouble naming the evidence", got)
	}

	// Seen at fixedNow and again 7h later, each as its own transfer (the clock
	// is keyed by infohash), every one of these stays healthy.
	cases := map[string]torrent{}
	queued := dead
	queued.Status = 3 // waiting in the client's queue, not fetching by choice
	cases["queued"] = queued
	stopped := dead
	stopped.Status = statusStopped
	cases["stopped"] = stopped
	complete := dead
	complete.PercentDone = 1
	complete.Status = statusSeeding
	cases["complete"] = complete
	i := 0
	for name, tor := range cases {
		i++
		tor.HashString = strings.Repeat(string(rune('b'+i)), 40)
		for _, at := range []time.Duration{0, 7 * time.Hour} {
			now = fixedNow.Add(at)
			if got := c.toTransfer(tor).Error; got != "" {
				t.Errorf("%s at +%s: reported %q, want healthy", name, at, got)
			}
		}
	}

	now = fixedNow
	off, err := New(Options{Name: "t", Endpoint: "http://transmission.invalid:9091", Label: "heyarr", Now: func() time.Time { return now }, StallAfter: -1})
	if err != nil {
		t.Fatal(err)
	}
	short, err := New(Options{Name: "t", Endpoint: "http://transmission.invalid:9091", Label: "heyarr", Now: func() time.Time { return now }, StallAfter: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	off.toTransfer(dead)
	short.toTransfer(dead)
	now = fixedNow.Add(2 * time.Hour)
	if got := off.toTransfer(dead).Error; got != "" {
		t.Errorf("stall_after \"0\" (off) still reported %q", got)
	}
	if got := short.toTransfer(dead).Error; !strings.HasPrefix(got, string(TroubleNoSeeders)) {
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

// A transfer that waited in the queue longer than the window has had no
// download slot yet: its window opens when it starts, not when it was added
// or first seen.
func TestTransmissionAQueuedTransferGetsAFullWindowOnceStarted(t *testing.T) {
	now := fixedNow
	c, err := New(Options{Name: "t", Endpoint: "http://transmission.invalid:9091", Label: "heyarr", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	tor := torrent{
		HashString: strings.Repeat("3", 40), Status: statusDownloading,
		AddedDate: fixedNow.Add(-1 * time.Hour).Unix(),
	}
	c.toTransfer(tor) // active, fetching nothing
	now = fixedNow.Add(5 * time.Hour)
	tor.Status = 3 // pushed back into the queue
	c.toTransfer(tor)
	now = fixedNow.Add(20 * time.Hour)
	tor.Status = statusDownloading // gets a slot again
	if got := c.toTransfer(tor).Error; got != "" {
		t.Fatalf("a transfer just given a slot reported %q", got)
	}
	now = fixedNow.Add(25 * time.Hour)
	if got := c.toTransfer(tor).Error; got != "" {
		t.Errorf("5h into its window reported %q, want healthy", got)
	}
	now = fixedNow.Add(27 * time.Hour)
	if got := c.toTransfer(tor).Error; !strings.HasPrefix(got, string(TroubleNoSeeders)+": no piece data for 7h") {
		t.Errorf("7h after it started reported %q, want no_seeders", got)
	}
}

func TestQBittorrentReportsADeadSwarmAfterTheWindow(t *testing.T) {
	now := fixedNow
	c, err := NewQBittorrent(QBOptions{Name: "qb", Endpoint: "http://qb.invalid:8080", Label: "heyarr", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	dead := qbTorrent{Hash: strings.Repeat("b", 40), State: "metaDL", AddedOn: fixedNow.Add(-8 * time.Hour).Unix()}
	paused := dead
	paused.Hash = strings.Repeat("c", 40)
	paused.State = "pausedDL"
	partial := qbTorrent{
		Hash: strings.Repeat("d", 40), State: "stalledDL", Completed: 400,
		AddedOn: fixedNow.Add(-10 * time.Hour).Unix(),
	}
	for _, tor := range []qbTorrent{dead, paused, partial} {
		if got := c.toTransfer(tor).Error; got != "" {
			t.Errorf("%s at first sight reported %q, want healthy", tor.State, got)
		}
	}
	now = fixedNow.Add(3 * time.Hour)
	partial.Completed = 500 // fetched: its window restarts
	if got := c.toTransfer(partial).Error; got != "" {
		t.Errorf("partial that fetched reported %q, want healthy", got)
	}
	now = fixedNow.Add(8 * time.Hour)
	if got := c.toTransfer(dead).Error; !strings.HasPrefix(got, string(TroubleNoSeeders)+": no piece data for 8h") {
		t.Errorf("a metaDL transfer idle for 8h reported %q, want no_seeders", got)
	}
	if got := c.toTransfer(paused).Error; got != "" {
		t.Errorf("paused: reported %q, want healthy", got)
	}
	now = fixedNow.Add(10 * time.Hour)
	if got := c.toTransfer(partial).Error; !strings.HasPrefix(got, string(TroubleNoSeeders)+": no piece data for 7h") {
		t.Errorf("partial idle for 7h reported %q, want no_seeders", got)
	}
}

func TestProgressClockStartsOverForANewAttempt(t *testing.T) {
	p := newProgressClock()
	added := fixedNow.Add(-time.Hour)
	p.lastProgress("x", 0, added, fixedNow)

	// Re-added under the same infohash (a new add date) with the same count.
	readded := fixedNow.Add(3 * time.Hour)
	if got := p.lastProgress("x", 0, readded, fixedNow.Add(4*time.Hour)); !got.Equal(fixedNow.Add(4 * time.Hour)) {
		t.Errorf("a re-added transfer kept window %v, want it to open at its first sight", got)
	}
	// Unseen for longer than forgetAfter, then back with the same add date: a
	// stale mark, never refreshed into a fresh-looking one.
	p.lastProgress("y", 0, added, fixedNow)
	back := fixedNow.Add(forgetAfter + time.Hour)
	if got := p.lastProgress("y", 0, added, back); !got.Equal(back) {
		t.Errorf("a transfer back after %s kept window %v, want it to open now", forgetAfter, got)
	}
	// Sweeping drops what no poll reports.
	p.lastProgress("z", 0, added, fixedNow.Add(2*forgetAfter+3*time.Hour))
	if _, ok := p.marks["x"]; ok {
		t.Error("a transfer unseen for longer than forgetAfter is still remembered")
	}
	if got := (*progressClock)(nil).lastProgress("x", 0, fixedNow, fixedNow); !got.IsZero() {
		t.Errorf("a nil clock returned %v, want the zero time", got)
	}
}
