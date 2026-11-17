package worker

import (
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// The dead-swarm fallback (#800, ADR-0119 #800 amendment), over #833's
// two-node harness: site A acquired a dump from a swarm and published its
// acquire. Site B fanned it out, its grab stalled, and the stuck-grab sweep
// blocked the release, so B records no_site_source. B reports that to A with
// an unsourced op; A answers with a hold; B pulls the bytes from A over the
// peer surface and records them under A's keys.

const deadSwarmInfohash = "cd" + "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"

var deadSwarmDump = map[string]string{"snes/Chrono Trigger (USA).sfc": strings.Repeat("ct", 5000)}

// deadSwarm is A and B after B's fan-out grab of A's release was blocked.
type deadSwarm struct {
	f       *transferFabric
	a, b    *site
	acquire string // the acquire op's hash
	workKey string
	want    string // B's fan-out want
}

// newDeadSwarm builds the state the fallback starts from. size is the release
// size A's acquire carries (0 = unknown). B knows A as a Partial Peer (the
// fabric) and A knows B the same way, by the key B signs its ops with.
func newDeadSwarm(t *testing.T, size int64) *deadSwarm {
	t.Helper()
	ctx := t.Context()
	f, a, b := twoSites(t)
	bPub := b.signer.Public().(ed25519.PublicKey)
	b.exec(`UPDATE peers SET public_key = ? WHERE is_self = 1`, []byte(bPub))
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	a.exec(`INSERT INTO peers (id, name, site, mode, is_self, public_key, endpoint, health, created_at, enrolled_at)
		VALUES (?, 'sibling', 'site-b', 'partial', 0, ?, '', 'reachable', ?, ?)`,
		uuid.Must(uuid.NewV7()).String(), []byte(bPub), stamp, stamp)

	// B wants roms under a profile of the name A's op carries, which takes
	// any dump: the profile is not what this test is about.
	b.exec(`INSERT INTO quality_profiles (id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES (?, 'rom-indexed', '', '[]', '[]', '[]', 1, ?, ?)`, uuid.Must(uuid.NewV7()).String(), stamp, stamp)

	a.scan(deadSwarmDump)
	wk := strings.Split(a.held()[0], "|")[0]
	tok, err := catalogop.SignAcquire(a.signer, identification.ROM, wk, catalogop.Acquire{
		Scope: catalogop.ScopeWork, Aspect: catalogop.AspectPrimary, Profile: "rom-indexed",
		Infohash: deadSwarmInfohash, Title: "Chrono Trigger (USA)", WorkTitle: "Chrono Trigger",
		Size: size, SelectedAt: peerPullNow, Site: a.siteName(), Trackers: []string{},
	}, nil, peerPullNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ops.RecordOps(ctx, []string{tok}); err != nil {
		t.Fatal(err)
	}
	d := &deadSwarm{f: f, a: a, b: b, acquire: catalogop.Hash(tok), workKey: wk}

	// A swarm covers the dump, so the origin publishes nothing for it.
	a.pass("roms")
	if n := a.count(`SELECT count(*) FROM catalog_ops`); n != 1 {
		t.Fatalf("the origin published %d ops beside its acquire for a swarm-covered dump", n-1)
	}

	// B fans it out: the want is created and the grab queued.
	carryOps(t, a, b)
	d.applyAcquire()
	app := d.application()
	if app.Outcome != catalog.FanOutApplied {
		t.Fatalf("B's first application = %+v, want applied", app)
	}
	d.want = app.DesiredItemID

	// Nobody seeds it. The stuck-grab sweep blocks the release and the want
	// falls back to idle; the op is re-evaluated and records no source.
	if _, err := b.cat.BlockRelease(ctx, catalog.BlockedRelease{
		DesiredItemID: d.want, Provider: catalog.FanOutProviderPrefix + a.siteName(),
		CandidateID: "infohash:" + deadSwarmInfohash, Title: "Chrono Trigger (USA)",
		Detail: "stalled", Reason: catalog.BlockGrabFailed,
	}); err != nil {
		t.Fatal(err)
	}
	b.exec(`UPDATE acquisition_state SET phase = 'idle', managed = 0 WHERE desired_item_id = ?`, d.want)
	b.exec(`DELETE FROM jobs`)
	d.applyAcquire()
	if app := d.application(); app.Outcome != catalog.FanOutNoSiteSource {
		t.Fatalf("B's application after the block = %+v, want no_site_source", app)
	}
	return d
}

// applyAcquire runs B's fan-out applier.
func (d *deadSwarm) applyAcquire() {
	d.b.t.Helper()
	selfKey := identity.FormatPublicKey(d.b.signer.Public().(ed25519.PublicKey))
	h := ApplyAcquireHandler(d.b.ops, d.b.cat, d.b.queue, selfKey, slog.New(slog.DiscardHandler))
	if err := h(d.b.t.Context(), jobs.Job{Type: acquisition.ApplyAcquireJobType}); err != nil {
		d.b.t.Fatalf("apply_acquire at B: %v", err)
	}
}

// application is B's recorded outcome for A's acquire.
func (d *deadSwarm) application() catalog.FanOutApplication {
	d.b.t.Helper()
	all, err := d.b.cat.FanOutApplications(d.b.t.Context())
	if err != nil {
		d.b.t.Fatal(err)
	}
	return all[d.acquire]
}

// reports are the unsourced ops in s's log.
func reports(s *site) []catalogop.Op {
	s.t.Helper()
	tokens, err := s.ops.Ops(s.t.Context())
	if err != nil {
		s.t.Fatal(err)
	}
	var out []catalogop.Op
	for _, op := range catalogop.Evaluate(tokens).Accepted {
		if op.Kind == catalogop.OpUnsourced {
			out = append(out, op)
		}
	}
	return out
}

// TestADeadSwarmsReleaseIsPulledFromItsOrigin is #800's acceptance.
func TestADeadSwarmsReleaseIsPulledFromItsOrigin(t *testing.T) {
	d := newDeadSwarm(t, 0)
	a, b, f := d.a, d.b, d.f

	// 1. B reports it cannot source A's release, once.
	b.pass("roms")
	got := reports(b)
	if len(got) != 1 || got[0].Unsourced.Acquire != d.acquire || got[0].Target.WorkKey != d.workKey {
		t.Fatalf("B's reports = %+v, want one naming A's acquire", got)
	}
	b.pass("roms")
	if n := len(reports(b)); n != 1 {
		t.Fatalf("B reported %d times, want once", n)
	}
	if f.sourceBlobs.requests.Load() != 0 {
		t.Fatal("bytes moved before the origin published anything")
	}

	// 2. A answers with a hold for the dump, and only for it.
	carryOps(t, b, a)
	a.pass("roms")
	tokens := carryOps(t, a, b)
	view := catalogop.Evaluate(tokens)
	if len(view.Holds) != 1 {
		t.Fatalf("A published %d holds, want one for the reported release", len(view.Holds))
	}
	var hold catalogop.Op
	for _, op := range view.Holds {
		hold = op
	}
	origin := strings.Split(a.held()[0], "|")
	if hold.Target.WorkKey != d.workKey || hold.Hold.Blob != origin[2] {
		t.Fatalf("hold = %s %s, want the reported work's blob %s", hold.Target.WorkKey, hold.Hold.Blob, origin[2])
	}
	if len(view.Heads) != 1 {
		t.Fatalf("the log has %d heads; reports and holds must chain", len(view.Heads))
	}
	f.sourceMans.hold(parseBlob(t, hold.Hold.Blob))

	// 3. B pulls it from A over the peer surface and records it on A's keys.
	b.pass("roms")
	if n := b.runPulls(f); n != 1 {
		t.Fatalf("B ran %d pulls, want 1", n)
	}
	if f.controllerHits.Load() != 0 {
		t.Fatal("bytes travelled through the controller")
	}
	if f.sourceBlobs.requests.Load() == 0 {
		t.Fatal("the origin served no blob read")
	}
	b.pass("roms")
	if got, want := b.held(), a.held(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the sibling holds %v, want the origin's %v", got, want)
	}

	// 4. The want holds its content, and the op settles held.
	if _, err := b.cat.ReconcileDesired(t.Context(), d.want); err != nil {
		t.Fatal(err)
	}
	d.applyAcquire()
	if app := d.application(); app.Outcome != catalog.FanOutHeld {
		t.Fatalf("B's application once the bytes arrived = %+v, want held", app)
	}

	// 5. get_acquisition_status at B says where the want came from and how it
	// was satisfied.
	status := b.tool("get_acquisition_status", `{"desired_item_id":"`+d.want+`"}`)
	fo, _ := status["fan_out"].(map[string]any)
	if fo == nil {
		t.Fatalf("acquisition status = %v, want a fan_out block", status)
	}
	if fo["origin_site"] != a.siteName() || fo["op_hash"] != d.acquire || fo["infohash"] != deadSwarmInfohash ||
		fo["outcome"] != "held" || fo["reported_unsourced"] != true {
		t.Fatalf("fan_out = %v", fo)
	}
	pulls, _ := fo["peer_pull"].([]any)
	if len(pulls) != 1 || pulls[0].(map[string]any)["outcome"] != "held" || pulls[0].(map[string]any)["library"] != "roms" {
		t.Fatalf("fan_out.peer_pull = %v, want one hold, held in roms", fo["peer_pull"])
	}

	// 6. Settled: further passes at either site sign, queue and record nothing.
	carryOps(t, b, a)
	opsA, opsB, evB := a.count(`SELECT count(*) FROM catalog_ops`), b.count(`SELECT count(*) FROM catalog_ops`), b.count(`SELECT count(*) FROM events`)
	a.pass("roms")
	b.pass("roms")
	d.applyAcquire()
	if a.count(`SELECT count(*) FROM catalog_ops`) != opsA || b.count(`SELECT count(*) FROM catalog_ops`) != opsB {
		t.Fatal("a settled pass signed another op")
	}
	if b.count(`SELECT count(*) FROM events`) != evB || b.count(`SELECT count(*) FROM jobs WHERE state = 'pending'`) != 0 {
		t.Fatal("a settled pass recorded or queued something at the sibling")
	}
}

// With the opt-in off at either site the fallback does nothing: B, off, signs
// no report and changes nothing; A, off, publishes nothing for a report it
// received.
func TestTheFallbackIsOffWithTheOptInOff(t *testing.T) {
	t.Run("at the sibling", func(t *testing.T) {
		d := newDeadSwarm(t, 0)
		tables := []string{"blobs", "replicas", "assets", "jobs", "events", "catalog_ops", "catalog_op_applications", "catalog_hold_applications"}
		before := map[string]int{}
		for _, tbl := range tables {
			before[tbl] = d.b.count(`SELECT count(*) FROM ` + tbl)
		}
		d.b.pass() // acquisition.peer_pull empty
		for _, tbl := range tables {
			if n := d.b.count(`SELECT count(*) FROM ` + tbl); n != before[tbl] {
				t.Fatalf("a pass with peer-pull off changed %s: %d -> %d", tbl, before[tbl], n)
			}
		}
		if app := d.application(); app.Outcome != catalog.FanOutNoSiteSource {
			t.Fatalf("B's application = %+v, want it left at no_site_source", app)
		}
	})
	t.Run("at the origin", func(t *testing.T) {
		d := newDeadSwarm(t, 0)
		d.b.pass("roms")
		carryOps(t, d.b, d.a)
		before := d.a.count(`SELECT count(*) FROM catalog_ops`)
		d.a.pass()
		if n := d.a.count(`SELECT count(*) FROM catalog_ops`); n != before {
			t.Fatalf("an origin with peer-pull off published %d ops for a report", n-before)
		}
	})
}

// Over the cap the fallback does nothing: a release whose size is over it is
// never reported, and a dump over it is never published, report or not.
func TestTheFallbackKeepsTheCap(t *testing.T) {
	t.Run("a release over the cap is not reported", func(t *testing.T) {
		d := newDeadSwarm(t, 1<<20)
		before := d.b.count(`SELECT count(*) FROM catalog_ops`)
		d.b.passWith(1<<10, "roms")
		if n := d.b.count(`SELECT count(*) FROM catalog_ops`); n != before {
			t.Fatalf("B signed %d ops for a release over its cap", n-before)
		}
	})
	t.Run("a dump over the cap is not published", func(t *testing.T) {
		d := newDeadSwarm(t, 0)
		d.b.pass("roms")
		carryOps(t, d.b, d.a)
		before := d.a.count(`SELECT count(*) FROM catalog_ops`)
		d.a.passWith(16, "roms")
		if n := d.a.count(`SELECT count(*) FROM catalog_ops`); n != before {
			t.Fatalf("A published %d ops for a dump over its cap", n-before)
		}
		if d.f.sourceBlobs.requests.Load() != 0 {
			t.Fatal("bytes moved for a dump over the cap")
		}
	})
}

// A report from a key that is not a member of the origin does not make it
// publish: the uncovering is the sibling's, and only a sibling's.
func TestAStrangersReportPublishesNothing(t *testing.T) {
	d := newDeadSwarm(t, 0)
	_, stranger, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := catalogop.SignUnsourced(stranger, identification.ROM, d.workKey,
		catalogop.Unsourced{Acquire: d.acquire, Site: "elsewhere"}, nil, peerPullNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.a.ops.RecordOps(t.Context(), []string{tok}); err != nil {
		t.Fatal(err)
	}
	before := d.a.count(`SELECT count(*) FROM catalog_ops`)
	d.a.pass("roms")
	if n := d.a.count(`SELECT count(*) FROM catalog_ops`); n != before {
		t.Fatalf("A published %d ops on a stranger's report", n-before)
	}
}

// A report is about one selection. If the origin has since upgraded the same
// scope to a newer release, that one covers the dump again until the sibling
// reports it in turn.
func TestANewerSelectionCoversUntilItIsReported(t *testing.T) {
	d := newDeadSwarm(t, 0)
	d.b.pass("roms")
	carryOps(t, d.b, d.a)

	heads, err := d.a.ops.Heads(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	newer, err := catalogop.SignAcquire(d.a.signer, identification.ROM, d.workKey, catalogop.Acquire{
		Scope: catalogop.ScopeWork, Aspect: catalogop.AspectPrimary, Profile: "rom-indexed",
		Infohash: strings.Repeat("ef", 20), Title: "Chrono Trigger (USA) (Rev 1)", WorkTitle: "Chrono Trigger",
		SelectedAt: peerPullNow.Add(time.Hour), Site: d.a.siteName(), Trackers: []string{},
	}, heads, peerPullNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.a.ops.RecordOps(t.Context(), []string{newer}); err != nil {
		t.Fatal(err)
	}
	before := d.a.count(`SELECT count(*) FROM catalog_ops`)
	d.a.pass("roms")
	if n := d.a.count(`SELECT count(*) FROM catalog_ops`); n != before {
		t.Fatalf("A published %d ops for a dump its newer, unreported selection covers", n-before)
	}
}
