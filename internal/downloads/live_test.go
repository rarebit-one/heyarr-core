package downloads

import (
	"context"
	"os"
	"testing"
	"time"
)

// The opt-in live exercise (ADR-0026).
//
// # Why this is opt-in rather than a container lane
//
// An earlier draft of M3-10 proposed a digest-pinned Transmission image behind
// `make integration`. That carried ADR-0023's FFmpeg analogy one step too far:
// pinning is for what Heyarr INSTALLS, and Heyarr installs nothing here. It
// would have bought one smoke test in exchange for two container runtimes to
// resolve, image digests to keep current, and a lane living off the merge path
// — and a gate nobody runs is a gate people stop running, which
// scripts/acceptance.sh already says about itself.
//
// So instead: one test, pointed at whatever you have. A container somebody
// started by hand, a daemon on a NAS, a laptop. It costs no infrastructure to
// own and it becomes a one-command verification the moment a real instance
// exists.
//
// # It is READ-ONLY
//
// session-get and torrent-get, nothing else. A live test that added a torrent
// would be mutating somebody's download client to satisfy a build, and the
// bytes it pulled would be somebody's bandwidth. The read path is where the
// parsing lives, which is what a live exercise is for; the write path is
// covered by the corpus and the fake.
//
// # Say what actually ran
//
// ADR-0026 requires each PR to record whether this executed and against what
// version. A skip is a fact worth reporting, not an absence worth glossing.
func TestLiveTransmission(t *testing.T) {
	endpoint := os.Getenv("HEYARR_TEST_TRANSMISSION_URL")
	if endpoint == "" {
		t.Skip("HEYARR_TEST_TRANSMISSION_URL is unset; " +
			"set it to exercise a real instance (read-only)")
	}

	client, err := New(Options{
		Name:     "live",
		Endpoint: endpoint,
		Username: os.Getenv("HEYARR_TEST_TRANSMISSION_USER"),
		Password: os.Getenv("HEYARR_TEST_TRANSMISSION_PASS"),
		Label:    os.Getenv("HEYARR_TEST_TRANSMISSION_LABEL"),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	health := client.Check(ctx)
	if !health.Healthy {
		t.Fatalf("the instance is not usable: %s", health.Detail)
	}
	// The conditions, reported. A result without them is a claim about a
	// benchmark.
	session := client.Session()
	t.Logf("live: version %s, rpc-version %d, labels %v, incomplete-dir %v",
		session.Version, session.RPCVersion,
		session.SupportsLabels(), session.IncompleteDirEnabled)

	// Read-only. Whatever is there is there; the assertion is that the client
	// can parse it, not that it contains anything in particular.
	transfers, err := client.Transfers(ctx)
	if err != nil {
		t.Fatalf("could not read the queue: %v", err)
	}
	t.Logf("live: %d transfer(s) carrying our label", len(transfers))

	for _, tr := range transfers {
		if tr.ID == "" {
			t.Errorf("a transfer came back with no identifier: %+v", tr)
		}
		if tr.Done && tr.Path == "" {
			t.Errorf("%s is complete and has no path", tr.Name)
		}
	}
}
