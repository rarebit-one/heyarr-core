package downloads

import (
	"context"
	"os"
	"testing"
	"time"
)

// The opt-in live exercise for SABnzbd (ADR-0026), the exact shape of
// TestLiveQBittorrent and TestLiveTransmission.
//
// # Why this is opt-in rather than a CI daemon
//
// ADR-0026 decided it, for the whole class: a download client is an
// operator-managed service Heyarr targets by configuration, not one it installs
// — so it is neither pinned (ADR-0023) nor run on the merge path. A SABnzbd in
// the acceptance lane is exactly the "digest-pinned container behind a lane
// nobody runs" that ADR rejected. The merge path tests this client against a
// fake of the HTTP API; the real exercise is this test, pointed at whatever
// instance you have and skipped when unset.
//
// # It is READ-ONLY, and there is no harness leg
//
// The version handshake and a queue/history read, nothing else. A live test that
// added an .nzb would be mutating somebody's client and spending their Usenet
// provider's bandwidth — the same line TestLiveQBittorrent draws. Unlike
// qBittorrent, there is ALSO no daemon-in-the-loop harness: proving a real
// SABnzbd transfer needs a real Usenet news server with the article posted (an
// NNTP + yEnc + .nzb stack), which has no clean disposable form the way
// qBittorrent's private web seed does. That leg is a documented follow-up (#379).
//
// # Say what actually ran
//
// ADR-0026 requires each PR to record whether this executed and against what
// version. A skip is a fact worth reporting, not an absence worth glossing.
func TestLiveSABnzbd(t *testing.T) {
	endpoint := os.Getenv("HEYARR_TEST_SABNZBD_URL")
	if endpoint == "" {
		t.Skip("HEYARR_TEST_SABNZBD_URL is unset; " +
			"set it to exercise a real instance (read-only)")
	}

	client, err := NewSABnzbd(SABOptions{
		Name:     "live",
		Endpoint: endpoint,
		APIKey:   os.Getenv("HEYARR_TEST_SABNZBD_APIKEY"),
		Label:    os.Getenv("HEYARR_TEST_SABNZBD_LABEL"),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	health := client.Check(ctx)
	t.Logf("live SABnzbd: healthy=%t version=%q detail=%q",
		health.Healthy, health.Version, health.Detail)
	if !health.Healthy {
		t.Fatalf("the live instance is not healthy: %s", health.Detail)
	}

	// Read-only: prove the parsing works against a real queue+history without
	// mutating it.
	transfers, err := client.Transfers(ctx)
	if err != nil {
		t.Fatalf("reading transfers from the live instance: %v", err)
	}
	t.Logf("live SABnzbd: %d transfer(s) in category %q", len(transfers), client.Label())
}
