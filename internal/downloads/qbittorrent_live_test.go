package downloads

import (
	"context"
	"os"
	"testing"
	"time"
)

// The opt-in live exercise for qBittorrent (ADR-0026), the exact shape of
// TestLiveTransmission.
//
// # Why this is opt-in rather than a CI daemon
//
// ADR-0026 decided it, for the whole class: an indexer and a download client are
// operator-managed services Heyarr targets by configuration, not things Heyarr
// installs — so neither is pinned (ADR-0023) nor run in CI. A qbittorrent-nox in
// the acceptance lane is exactly the "digest-pinned container behind a lane
// nobody runs" that ADR rejected. The merge path tests this client against a
// fake of the Web API; the real exercise is this test, pointed at whatever
// instance you have and skipped when unset.
//
// # It is READ-ONLY
//
// The version handshake and a torrents/info read, nothing else. A live test that
// added a torrent would be mutating somebody's client to satisfy a build, and
// the bytes it pulled would be their bandwidth — the same line TestLiveTransmission
// draws. The add path is covered by the fake.
//
// # Say what actually ran
//
// ADR-0026 requires each PR to record whether this executed and against what
// version. A skip is a fact worth reporting, not an absence worth glossing.
func TestLiveQBittorrent(t *testing.T) {
	endpoint := os.Getenv("HEYARR_TEST_QBITTORRENT_URL")
	if endpoint == "" {
		t.Skip("HEYARR_TEST_QBITTORRENT_URL is unset; " +
			"set it to exercise a real instance (read-only)")
	}

	client, err := NewQBittorrent(QBOptions{
		Name:     "live",
		Endpoint: endpoint,
		Username: os.Getenv("HEYARR_TEST_QBITTORRENT_USER"),
		Password: os.Getenv("HEYARR_TEST_QBITTORRENT_PASS"),
		Label:    os.Getenv("HEYARR_TEST_QBITTORRENT_LABEL"),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	health := client.Check(ctx)
	t.Logf("live qBittorrent: healthy=%t version=%q detail=%q",
		health.Healthy, health.Version, health.Detail)
	if !health.Healthy {
		t.Fatalf("the live instance is not healthy: %s", health.Detail)
	}

	// Read-only: prove the parsing works against a real queue without mutating it.
	transfers, err := client.Transfers(ctx)
	if err != nil {
		t.Fatalf("reading transfers from the live instance: %v", err)
	}
	t.Logf("live qBittorrent: %d transfer(s) in category %q", len(transfers), client.Label())
}
