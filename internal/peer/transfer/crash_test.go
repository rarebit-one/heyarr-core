package transfer_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// Crash, not cancel (§84, ADR-0035, M5-06).
//
// A cancelled context unwinds: deferred closes run, buffers flush, the staging
// file is left in a state the process chose. A SIGKILL is what actually
// happens — the machine loses power, the OOM killer picks the worker, an
// operator restarts a container — and it leaves whatever the kernel had
// written and nothing else. Every guarantee in ADR-0035 is about that file, so
// the test that matters has to produce one that way.
//
// The transfer therefore runs in a REAL subprocess against a REAL peer
// surface, and the parent kills it with SIGKILL part-way through.

// childEnv makes this test binary run as the child rather than as a test.
const childEnv = "HEYARR_TEST_CHUNKED_PULL_CHILD"

// Environment the child is handed. It is given endpoints and keys — never a
// manifest and never an offset: the child fetches the manifest itself and
// re-derives what it has from the bytes on disk, which is the whole point.
const (
	envCASRoot    = "HEYARR_TEST_CAS_ROOT"
	envOrigin     = "HEYARR_TEST_SOURCE_ORIGIN"
	envSourceID   = "HEYARR_TEST_SOURCE_PEER_ID"
	envSourceKey  = "HEYARR_TEST_SOURCE_PUBKEY"
	envSelfID     = "HEYARR_TEST_SELF_PEER_ID"
	envSelfSeed   = "HEYARR_TEST_SELF_SEED"
	envBlobDigest = "HEYARR_TEST_BLOB"
)

// TestChunkedPullChildProcess is the subprocess entry point, not an assertion.
//
// It returns immediately unless this binary was started as the child, which is
// the standard shape for a test that needs a process of its own. It is not
// t.Skip: a skip would appear in `make test-skips` as coverage somebody chose
// not to run, and this is the opposite — it is machinery the test above
// depends on and always exercises.
func TestChunkedPullChildProcess(t *testing.T) {
	if os.Getenv(childEnv) != "1" {
		return
	}
	if err := runChildPull(); err != nil {
		// Written to stderr and exited non-zero so a child that failed for a
		// reason other than being killed is diagnosable rather than looking
		// like a successful kill.
		fmt.Fprintf(os.Stderr, "child pull: %v\n", err)
		os.Exit(2)
	}
	os.Exit(0)
}

func runChildPull() error {
	seed, err := hex.DecodeString(os.Getenv(envSelfSeed))
	if err != nil {
		return err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	material, err := mtls.NewMaterial(mtls.MaterialOptions{
		PrivateKey: priv, PeerID: os.Getenv(envSelfID), Lifetime: time.Hour, RenewBefore: time.Minute,
	})
	if err != nil {
		return err
	}
	store, err := cas.OpenFS(os.Getenv(envCASRoot))
	if err != nil {
		return err
	}
	puller, err := transfer.New(transfer.Options{Material: material, Store: store})
	if err != nil {
		return err
	}
	pub, err := hex.DecodeString(os.Getenv(envSourceKey))
	if err != nil {
		return err
	}
	blob, err := hashing.Parse(os.Getenv(envBlobDigest))
	if err != nil {
		return err
	}
	src := replication.Source{
		PeerID:   os.Getenv(envSourceID),
		Name:     "source",
		Endpoint: os.Getenv(envOrigin),
		// PublicKey is what the client pins to, so the child trusts exactly
		// the peer the parent enrolled it with and nothing that answers at
		// that address.
		PublicKey: ed25519.PublicKey(pub),
	}
	ctx := context.Background()
	m, err := puller.FetchManifest(ctx, src, blob)
	if err != nil {
		return err
	}
	_, err = puller.PullChunked(ctx, src, blob, m)
	return err
}

// 🔴 A process killed mid-transfer leaves bytes a later attempt re-verifies
// and resumes from.
func TestAKilledProcessResumesFromWhatIsOnDisk(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a subprocess and waits on real elapsed time")
	}
	selfPub, selfPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	selfMaterial, err := mtls.NewMaterial(mtls.MaterialOptions{
		PrivateKey: selfPriv, PeerID: "peer-destination", Lifetime: time.Hour, RenewBefore: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	self := &node{peerID: "peer-destination", name: "destination", pub: selfPub, material: selfMaterial}
	src := newNode(t, "peer-source", "source")
	root := newTrustRoot(src.member(), self.member())

	content := deterministicContent(61, 512<<10)
	source := startChunkedSource(t, src, root, content)
	blob, m := manifestOf(t, content)
	source.mans.store(m)
	// Slowed so the child is reliably still transferring when it is killed. A
	// kill that lands after the transfer finished would assert nothing.
	source.counting.delay = 3 * time.Millisecond

	casRoot := t.TempDir()
	if _, err := cas.OpenFS(casRoot); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestChunkedPullChildProcess", "-test.timeout=2m") // #nosec G204 -- this test binary
	cmd.Env = append(os.Environ(),
		childEnv+"=1",
		envCASRoot+"="+casRoot,
		envOrigin+"=https://"+source.addr,
		envSourceID+"="+src.peerID,
		envSourceKey+"="+hex.EncodeToString(src.pub),
		envSelfID+"="+self.peerID,
		envSelfSeed+"="+hex.EncodeToString(selfPriv.Seed()),
		envBlobDigest+"="+blob.String(),
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	// Wait until the child has staged a real prefix, then kill it. Polling on
	// the FILE rather than on elapsed time is what keeps this deterministic on
	// a loaded machine.
	partial := casRoot + "/tmp/" + cas.PartialName(blob)
	want := int64(len(content)) / 4
	deadline := time.Now().Add(60 * time.Second)
	var staged int64
	for time.Now().Before(deadline) {
		if info, err := os.Stat(partial); err == nil {
			staged = info.Size()
			if staged >= want {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if staged < want {
		t.Fatalf("the child staged %d bytes in 60s, wanted %d before killing it", staged, want)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	killed = true
	_ = cmd.Wait()

	afterKill := int64(0)
	if info, err := os.Stat(partial); err == nil {
		afterKill = info.Size()
	}
	if afterKill == 0 {
		t.Fatal("the killed process left nothing on disk, so there is nothing to resume from")
	}
	if afterKill >= int64(len(content)) {
		t.Fatalf("the killed process left %d bytes of a %d byte blob, which is not a partial",
			afterKill, len(content))
	}
	if held, err := cas.OpenFS(casRoot); err != nil {
		t.Fatal(err)
	} else if present, err := held.Has(t.Context(), blob); err != nil || present {
		t.Errorf("the blob is present after a killed transfer: %v (err %v)", present, err)
	}

	// Resume in this process, over the same CAS root and against the same
	// source. Nothing was handed over: the resume re-reads the file and
	// re-hashes it against the manifest.
	source.counting.reset()
	source.counting.delay = 0
	dest := openChunkedDestination(t, self, casRoot)
	out, err := dest.puller.PullChunked(t.Context(), source.source(), blob, m)
	if err != nil {
		t.Fatalf("resuming after a kill: %v", err)
	}
	served, _, _ := source.counting.stats()

	if out.ChunksKept == 0 {
		t.Error("the resume after a kill kept no chunks, so it was a whole retry")
	}
	if served >= int64(len(content)) {
		t.Errorf("the resume served %d bytes of a %d byte blob, so nothing was saved",
			served, len(content))
	}
	if got := readBlob(t, dest.store, blob); !bytes.Equal(got, content) {
		t.Error("the blob published after a kill and a resume is not the source's content")
	}
	if err := dest.store.Verify(t.Context(), blob); err != nil {
		t.Errorf("the blob published after a kill does not verify: %v", err)
	}
	t.Logf("crash: the killed child left %d bytes of %d; the resume kept %d/%d chunks and the "+
		"source served %d bytes (%.1f%% of the blob)",
		afterKill, len(content), out.ChunksKept, len(m.Chunks), served,
		100*float64(served)/float64(len(content)))
}
