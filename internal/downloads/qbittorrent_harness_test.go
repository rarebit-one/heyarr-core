package downloads

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// TestHarnessQBittorrentTransfer is the daemon-in-the-loop acceptance for the
// qBittorrent client (#379): a REAL qBittorrent, standing up in a container the
// harness owns, driven through the whole arc the fake stands in for — connect,
// authenticate, add a transfer, watch it move through real qBittorrent states,
// and COMPLETE — with the bytes that land proven byte-identical to what was
// offered.
//
// # Why this is a harness test and not the opt-in TestLiveQBittorrent
//
// TestLiveQBittorrent (ADR-0026) points at an operator's OWN instance and is
// deliberately READ-ONLY: adding a torrent there would mutate somebody's client
// and spend somebody's bandwidth. That read-only ceiling is exactly why it can
// never prove `acquires-over-daemon-clients` — "a real qBittorrent transfer
// completed" — no matter how many times it runs.
//
// This test removes the ceiling by removing the objection: the qBittorrent is
// DISPOSABLE and the harness's own, and the payload is served by a web seed on
// the same private network, so a full add-through-complete round trip costs no
// operator anything and reaches no third party. That is the amendment ADR-0052
// makes to ADR-0026 for a second download client — the revisit trigger ADR-0026
// itself named.
//
// # Why it skips rather than fails when unset
//
// It runs ONLY when the harness has put a real endpoint in the environment
// (scripts/daemon-acceptance.sh, and the scheduled daemon-acceptance workflow).
// On the ordinary merge path `go test ./...` finds the variables unset and
// skips — the daemon leg is out of the demo budget by design (ADR-0026), and a
// container pull has no place gating a pull request. What DOES run on the merge
// path is TestMakeWebseedTorrent, which proves the one algorithmic part.
//
//	HEYARR_HARNESS_QBITTORRENT_URL   the disposable qBittorrent Web API base URL
//	HEYARR_HARNESS_WEBSEED_DIR       a dir the web-seed server serves AND this
//	                                 test can write the payload + .torrent into
//	HEYARR_HARNESS_WEBSEED_BASEURL   the URL prefix qBittorrent uses to reach it
//	HEYARR_HARNESS_DOWNLOAD_DIR      the host path qBittorrent's save dir is
//	                                 bind-mounted to, so completed bytes are
//	                                 readable here
//	HEYARR_HARNESS_QBITTORRENT_USER  / _PASS  the WebUI credential, if any
func TestHarnessQBittorrentTransfer(t *testing.T) {
	endpoint := os.Getenv("HEYARR_HARNESS_QBITTORRENT_URL")
	webseedDir := os.Getenv("HEYARR_HARNESS_WEBSEED_DIR")
	webseedBase := os.Getenv("HEYARR_HARNESS_WEBSEED_BASEURL")
	downloadDir := os.Getenv("HEYARR_HARNESS_DOWNLOAD_DIR")
	if endpoint == "" || webseedDir == "" || webseedBase == "" || downloadDir == "" {
		t.Skip("qBittorrent harness is not configured; run scripts/daemon-acceptance.sh " +
			"(sets HEYARR_HARNESS_QBITTORRENT_URL and the web-seed/download paths)")
	}

	// The save path qBittorrent writes to, as qBittorrent sees it INSIDE its
	// container, is mapped to downloadDir as this test sees it on the host — the
	// path map is part of what the harness proves, since a wrong map is the most
	// common operational failure in this class of software (see resolvePath).
	remoteSave := os.Getenv("HEYARR_HARNESS_REMOTE_SAVE")
	if remoteSave == "" {
		remoteSave = "/downloads"
	}
	pathMap, err := ParsePathMap("harness", []Mapping{{Remote: remoteSave, Local: downloadDir}})
	if err != nil {
		t.Fatalf("path map: %v", err)
	}

	client, err := NewQBittorrent(QBOptions{
		Name:     "harness",
		Endpoint: endpoint,
		Username: os.Getenv("HEYARR_HARNESS_QBITTORRENT_USER"),
		Password: os.Getenv("HEYARR_HARNESS_QBITTORRENT_PASS"),
		PathMap:  pathMap,
		Label:    DefaultLabel,
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 1. Connect + authenticate against the real daemon. The demo only reaches a
	//    Check against an endpoint that REFUSES; this reaches one that answers.
	health := client.Check(ctx)
	if !health.Healthy {
		t.Fatalf("the harness qBittorrent is not healthy: %s", health.Detail)
	}
	t.Logf("harness qBittorrent: healthy=%t version=%q", health.Healthy, health.Version)

	// 2. Build a real .torrent whose only source is a web seed on the private
	//    network, and publish it where qBittorrent can fetch both the metainfo
	//    and the payload. No tracker, no peer, no DHT — deterministic.
	payload := harnessPayload()
	torrent, infoHash, err := makeWebseedTorrent(
		"heyarr-harness.bin", payload, 16*1024, webseedBase+"/heyarr-harness.bin")
	if err != nil {
		t.Fatalf("building the torrent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webseedDir, "heyarr-harness.bin"), payload, 0o644); err != nil {
		t.Fatalf("publishing the payload: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webseedDir, "heyarr-harness.torrent"), torrent, 0o644); err != nil {
		t.Fatalf("publishing the torrent: %v", err)
	}

	// 3. Add it through the ordinary client path. qBittorrent fetches the
	//    .torrent from the web seed and takes it into our category.
	//
	//    The transfer's identity is the v1 infohash, which the generator already
	//    handed back — so the arc below keys on that rather than on Add's return.
	//    qBittorrent registers a URL-sourced .torrent asynchronously, so Add can
	//    legitimately return before the queue shows it; that timing is not what
	//    this harness is proving, and keying on the known infohash sidesteps it
	//    without hiding a real failure (a rejected source simply never appears).
	wantID := strings.ToLower(infoHash)
	added, err := client.Add(ctx, secret.Value(webseedBase+"/heyarr-harness.torrent"))
	if err != nil {
		t.Logf("harness qBittorrent: Add returned %v (tolerated; proof is the infohash appearing and completing)", err)
	} else {
		t.Logf("harness qBittorrent: added transfer id=%q name=%q", added.ID, added.Name)
	}

	// 4. Watch it through real qBittorrent state, and require it to COMPLETE.
	//    A transfer that is Done AND carries a resolved path (only set on
	//    completion, see toTransfer) is the honest end of the arc.
	var final providers.Transfer
	appeared := false
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		transfers, err := client.Transfers(ctx)
		if err != nil {
			t.Fatalf("reading transfers: %v", err)
		}
		var mine *providers.Transfer
		for i := range transfers {
			if strings.EqualFold(transfers[i].ID, wantID) {
				mine = &transfers[i]
			}
		}
		if mine == nil {
			// Not registered yet, or gone. Before it has ever appeared this is
			// the async add still settling; keep waiting.
			time.Sleep(time.Second)
			continue
		}
		appeared = true
		if mine.Error != "" {
			t.Fatalf("qBittorrent reported an error on the transfer: %s", mine.Error)
		}
		final = *mine
		if final.Done {
			break
		}
		time.Sleep(time.Second)
	}
	if !appeared {
		t.Fatalf("qBittorrent never registered the transfer %s in our category — it did not accept the source", wantID)
	}
	if !final.Done {
		t.Fatalf("the transfer did not complete within the budget (last: done=%t bytesDone=%d/%d)",
			final.Done, final.BytesDone, final.BytesTotal)
	}
	if final.Path == "" {
		t.Fatal("the completed transfer has no resolved path")
	}
	t.Logf("harness qBittorrent: transfer completed, resolved path %q", final.Path)

	// 5. The bytes that landed are byte-identical to what was offered. This is
	//    the same standard the plain-HTTP and OpenSubsonic scenes hold: a real
	//    caller drove a real fetch, proven by the bytes rather than by a status
	//    code. The path came out of the client's path map, so a broken map fails
	//    here.
	got, err := os.ReadFile(final.Path)
	if err != nil {
		// content_path may name the file directly or a directory; fall back to
		// the file under the resolved directory.
		alt := filepath.Join(final.Path, "heyarr-harness.bin")
		got, err = os.ReadFile(alt)
		if err != nil {
			t.Fatalf("reading the completed bytes at %q (and %q): %v", final.Path, alt, err)
		}
	}
	if len(got) != len(payload) {
		t.Fatalf("completed file is %d bytes, offered %d", len(got), len(payload))
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("completed bytes differ from what was offered at byte %d", i)
		}
	}
	t.Logf("HARNESS: a real qBittorrent transfer completed and the bytes are byte-identical (infohash %s)", infoHash)

	// Leave the client's queue as we found it.
	if err := client.Remove(ctx, wantID, true); err != nil {
		t.Logf("cleanup: could not remove the transfer (non-fatal): %v", err)
	}
}

// harnessPayload is deterministic bytes, larger than one piece so completion
// exercises multi-piece assembly rather than a single-shot fetch.
func harnessPayload() []byte {
	const size = 512 * 1024
	out := make([]byte, size)
	for i := range out {
		out[i] = byte((i*31 + 7) % 251)
	}
	return out
}
