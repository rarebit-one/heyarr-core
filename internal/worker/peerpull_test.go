package worker

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/api/mcp"
	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/catalogtomb"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// The two-node acceptance for #833 (ADR-0119 amendment): a `rom` library
// scanned at one site shows the same works held at its sibling, with matching
// blob hashes, and get_replica_status reports them.
//
// Site A is the origin: its own database, catalog, op-log and content store,
// whose bytes it serves over a real mTLS peer surface (the transfer fabric's
// source). Site B is the fabric's destination, which pulls with a real puller.
// Each site signs with its own identity key, and B knows A only as an enrolled
// Partial Peer, as ADR-0119 has the siblings. The op-log exchange is the
// catalog sync's (one RecordOps of the other's log), whose own tests cover it
// over the wire.

var peerPullNow = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

// site is one node's catalog side, over its own database.
type site struct {
	t        *testing.T
	db       *sqlite.DB
	events   *events.Log
	cat      *catalog.Catalog
	ops      *catalogtomb.Store
	pipeline *ingest.Pipeline
	queue    *jobs.Queue
	store    *cas.FS
	signer   ed25519.PrivateKey
	rootDir  string
	rootID   string
}

func newSite(t *testing.T, db *sqlite.DB, cat *catalog.Catalog, store *cas.FS, signer ed25519.PrivateKey) *site {
	t.Helper()
	ev, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	if cat == nil {
		if cat, err = catalog.New(catalog.Options{DB: db, Events: ev, PeerName: "origin", PeerSite: "site-a"}); err != nil {
			t.Fatal(err)
		}
	}
	ops, err := catalogtomb.New(catalogtomb.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := ingest.New(ingest.Options{
		Store: NewCASByteStore(store), Catalog: cat, Identifier: identification.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: ev})
	if err != nil {
		t.Fatal(err)
	}
	s := &site{
		t: t, db: db, events: ev, cat: cat, ops: ops, pipeline: pipeline, queue: queue,
		store: store, signer: signer, rootDir: filepath.Join(t.TempDir(), "roms"),
	}
	if err := os.MkdirAll(s.rootDir, 0o750); err != nil {
		t.Fatal(err)
	}
	s.rootID = s.addLibrary("roms", identification.ROM, s.rootDir)
	return s
}

func (s *site) addLibrary(name, contentType, path string) string {
	s.t.Helper()
	libID, rootID := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s.exec(`INSERT INTO libraries (id, name, content_type, created_at) VALUES (?, ?, ?, ?)`, libID, name, contentType, now)
	s.exec(`INSERT INTO library_roots (id, library_id, path, ingest_mode, created_at) VALUES (?, ?, ?, 'copy', ?)`,
		rootID, libID, path, now)
	return rootID
}

func (s *site) exec(query string, args ...any) {
	s.t.Helper()
	if _, err := s.db.Writer().Exec(query, args...); err != nil {
		s.t.Fatalf("(%s): %v", query, err)
	}
}

// scan is the library scan's per-file step: each file under the root through
// the ingest pipeline, as the scanner's ingest jobs run it.
func (s *site) scan(files map[string]string) {
	s.t.Helper()
	for rel, body := range files {
		full := filepath.Join(s.rootDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			s.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o640); err != nil {
			s.t.Fatal(err)
		}
		if _, err := s.pipeline.Ingest(s.t.Context(), ingest.Request{RootID: s.rootID, SourcePath: full, RelPath: rel}); err != nil {
			s.t.Fatalf("ingesting %s: %v", rel, err)
		}
	}
}

func (s *site) pass(libraries ...string) {
	s.t.Helper()
	s.passWith(config.DefaultPeerPullMaxBlobBytes, libraries...)
}

func (s *site) passWith(maxBytes int64, libraries ...string) {
	s.t.Helper()
	h := PeerPullHandler(PeerPull{
		Ops: s.ops, Catalog: s.cat, Store: s.store, Adopter: s.pipeline, Queue: s.queue,
		Signer: s.signer, Site: s.siteName(), Libraries: libraries, MaxBlobBytes: maxBytes,
		Now: func() time.Time { return peerPullNow }, Log: slog.New(slog.DiscardHandler),
	})
	if err := h(s.t.Context(), jobs.Job{Type: replication.PeerPullJobType}); err != nil {
		s.t.Fatalf("peer-pull pass: %v", err)
	}
}

func (s *site) siteName() string {
	var name string
	if err := s.db.Reader().QueryRow(`SELECT site FROM peers WHERE is_self = 1`).Scan(&name); err != nil {
		s.t.Fatal(err)
	}
	return name
}

// held is every rom asset this site holds as "work_key|edition_key|blob", in
// sorted order.
func (s *site) held() []string {
	s.t.Helper()
	rows, err := s.db.Reader().Query(`
		SELECT w.work_key, e.edition_key, a.blob_hash FROM assets a
		  JOIN editions e ON e.id = a.edition_id JOIN works w ON w.id = e.work_id
		 WHERE w.content_type = 'rom' ORDER BY 1, 2, 3`)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var wk, ek, blob string
		if err := rows.Scan(&wk, &ek, &blob); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, wk+"|"+ek+"|"+blob)
	}
	return out
}

func (s *site) count(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.db.Reader().QueryRow(query, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// runPulls runs every pending replicate_blob in this site's queue with the
// fabric's real puller, and marks each one done.
func (s *site) runPulls(f *transferFabric) int {
	s.t.Helper()
	todo := s.pendingPulls()
	for _, p := range todo {
		job := jobs.Job{ID: p.id, Type: replication.ReplicateBlobJobType, Payload: json.RawMessage(p.payload)}
		if err := f.handler()(s.t.Context(), job); err != nil {
			s.t.Fatalf("replicate_blob %s: %v", p.payload, err)
		}
		s.exec(`UPDATE jobs SET state = 'succeeded' WHERE id = ?`, p.id)
	}
	return len(todo)
}

type pendingPull struct{ id, payload string }

// pendingPulls reads the queue's pending replicate_blob jobs, closing the rows
// before any of them runs (the single-writer pool would otherwise wait on them).
func (s *site) pendingPulls() []pendingPull {
	s.t.Helper()
	rows, err := s.db.Reader().Query(`SELECT id, payload FROM jobs WHERE type = ? AND state = 'pending' ORDER BY id`,
		replication.ReplicateBlobJobType)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []pendingPull
	for rows.Next() {
		var p pendingPull
		if err := rows.Scan(&p.id, &p.payload); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// replicaStatus calls the MCP tool an operator or an agent would ask.
func (s *site) replicaStatus(blob string) map[string]any {
	s.t.Helper()
	return s.tool("get_replica_status", `{"blob_hash":"`+blob+`"}`)
}

// tool calls one MCP tool at this site, as an operator or an agent would, and
// returns its reply as JSON.
func (s *site) tool(name, args string) map[string]any {
	s.t.Helper()
	tokens, err := auth.NewStore(auth.StoreOptions{Writer: s.db.Writer(), Reader: s.db.Reader()})
	if err != nil {
		s.t.Fatal(err)
	}
	api, err := resources.New(resources.Options{
		DB: s.db, Jobs: s.queue, Events: s.events, Tokens: tokens, Catalog: s.cat,
		Logger: slog.New(slog.DiscardHandler), Now: time.Now,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	srv, err := mcp.New(mcp.Options{DB: s.db, Resources: api, Jobs: s.queue})
	if err != nil {
		s.t.Fatal(err)
	}
	for _, tool := range srv.Tools() {
		if tool.Name != name {
			continue
		}
		out, err := tool.Handler(s.t.Context(), json.RawMessage(args))
		if err != nil {
			s.t.Fatal(err)
		}
		raw, err := json.Marshal(out)
		if err != nil {
			s.t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			s.t.Fatal(err)
		}
		return m
	}
	s.t.Fatalf("no %s tool", name)
	return nil
}

// twoSites is the origin A and the sibling B over one transfer fabric: A's
// store is the fabric's source store, served by its peer surface, and B is the
// fabric's destination, which knows A as an enrolled Partial Peer.
func twoSites(t *testing.T) (f *transferFabric, a, b *site) {
	t.Helper()
	f = newTransferFabric(t)
	f.exec(`UPDATE peers SET mode = 'partial' WHERE id = ?`, f.sourceID)

	a = newSite(t, testdb.Migrated(t), nil, f.sourceStore, f.sourceKey)
	self, err := a.cat.SelfPeer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	a.exec(`UPDATE peers SET public_key = ? WHERE id = ?`, []byte(f.sourceKey.Public().(ed25519.PublicKey)), self)

	_, bKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b = newSite(t, f.db, f.cat, f.store, bKey)
	return f, a, b
}

// carryOps carries A's op-log to B, as the catalog-ops sync does.
func carryOps(t *testing.T, a, b *site) []string {
	t.Helper()
	tokens, err := a.ops.Ops(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ops.RecordOps(t.Context(), tokens); err != nil {
		t.Fatal(err)
	}
	return tokens
}

var romLibrary = map[string]string{
	"snes/Super Mario World (USA).sfc":   strings.Repeat("smw", 4000),
	"snes/Chrono Trigger (USA).sfc":      strings.Repeat("ct", 5000),
	"gba/Metroid Fusion (Europe).gba":    strings.Repeat("mf", 3000),
	"gba/Advance Wars (USA, Europe).gba": strings.Repeat("aw", 2000),
}

// TestARomLibraryScannedAtOneSiteIsHeldAtItsSibling is #833's acceptance.
func TestARomLibraryScannedAtOneSiteIsHeldAtItsSibling(t *testing.T) {
	f, a, b := twoSites(t)
	ctx := t.Context()
	a.scan(romLibrary)
	origin := a.held()
	if len(origin) != len(romLibrary) {
		t.Fatalf("the origin's scan identified %d of %d dumps: %v", len(origin), len(romLibrary), origin)
	}

	// One work came by a swarm: an acquire covers it, so it is not published.
	acquired := strings.Split(origin[0], "|")[0]
	acq, err := catalogop.SignAcquire(a.signer, "rom", acquired, catalogop.Acquire{
		Scope: catalogop.ScopeWork, Aspect: catalogop.AspectPrimary, Profile: "rom-indexed",
		Infohash: strings.Repeat("ab", 20), Title: "a release", WorkTitle: "A Game",
		SelectedAt: peerPullNow, Site: "site-a", Trackers: []string{},
	}, nil, peerPullNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ops.RecordOps(ctx, []string{acq}); err != nil {
		t.Fatal(err)
	}

	a.pass("roms")
	tokens := carryOps(t, a, b)
	view := catalogop.Evaluate(tokens)
	if got := len(view.Holds); got != len(romLibrary)-1 {
		t.Fatalf("published %d holds, want one per scanned dump but the acquired one (%d)", got, len(romLibrary)-1)
	}
	if len(view.Heads) != 1 {
		t.Fatalf("the log has %d heads after publishing; holds must chain so the next op can cite them", len(view.Heads))
	}
	for _, op := range view.Holds {
		if op.Target.WorkKey == acquired {
			t.Fatalf("published a hold for %s, which an acquire covers", acquired)
		}
		if strings.HasPrefix(op.Hold.Path, "/") || !strings.Contains(op.Hold.Path, "/") {
			t.Fatalf("hold path %q is not the root-relative path", op.Hold.Path)
		}
		f.sourceMans.hold(parseBlob(t, op.Hold.Blob)) // the source holds it and has no manifest
	}

	b.pass("roms")
	if n := b.count(`SELECT count(*) FROM catalog_hold_applications WHERE outcome = 'pulling'`); n != len(view.Holds) {
		t.Fatalf("%d holds pulling at B, want %d", n, len(view.Holds))
	}
	if n := b.runPulls(f); n != len(view.Holds) {
		t.Fatalf("B ran %d pulls, want %d", n, len(view.Holds))
	}
	if f.controllerHits.Load() != 0 {
		t.Fatal("bytes travelled through the controller")
	}
	if served := f.sourceBlobs.requests.Load(); served < int64(len(view.Holds)) {
		t.Fatalf("the origin served %d blob reads for %d holds", served, len(view.Holds))
	}
	b.pass("roms")

	var want []string
	for _, h := range origin {
		if !strings.HasPrefix(h, acquired+"|") {
			want = append(want, h)
		}
	}
	sort.Strings(want)
	got := b.held()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the sibling holds\n%s\nwant the origin's works, editions and blobs\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := b.count(`SELECT count(*) FROM assets WHERE identification_source = ?`, identification.SourceSiblingHold); n != len(want) {
		t.Fatalf("%d assets say they came from a sibling's hold, want %d", n, len(want))
	}
	if n := b.count(`SELECT count(*) FROM catalog_hold_applications WHERE outcome = 'held'`); n != len(want) {
		t.Fatalf("%d holds held at B, want %d", n, len(want))
	}

	// get_replica_status at B: this node and the origin hold the blob, and its
	// hold is reported held, in the library it landed in.
	blob := strings.Split(want[0], "|")[2]
	status := b.replicaStatus(blob)
	states := map[string]string{}
	for _, r := range status["replicas"].([]any) {
		row := r.(map[string]any)
		states[row["peer_id"].(string)] = row["state"].(string)
	}
	if states[f.self] != "present" || states[f.sourceID] != "present" {
		t.Fatalf("replicas = %v, want this node and the origin present", states)
	}
	holds, _ := status["peer_pull"].([]any)
	if len(holds) != 1 || holds[0].(map[string]any)["outcome"] != "held" || holds[0].(map[string]any)["library"] != "roms" {
		t.Fatalf("peer_pull = %v, want one hold, held in roms", status["peer_pull"])
	}

	// Idempotent: another pass at either site publishes, queues and records
	// nothing (invariant 9), and the sibling publishes nothing of what it
	// pulled, so nothing echoes back to the origin.
	opsBefore, evBefore := a.count(`SELECT count(*) FROM catalog_ops`), b.count(`SELECT count(*) FROM events`)
	a.pass("roms")
	b.pass("roms")
	if a.count(`SELECT count(*) FROM catalog_ops`) != opsBefore {
		t.Fatal("the origin published again")
	}
	if b.count(`SELECT count(*) FROM events`) != evBefore || b.count(`SELECT count(*) FROM jobs WHERE state = 'pending'`) != 0 {
		t.Fatal("the sibling recorded or queued something on a settled pass")
	}
	if n := b.count(`SELECT count(*) FROM catalog_ops`); n != len(tokens) {
		t.Fatalf("the sibling published %d ops of its own", n-len(tokens))
	}
}

// With no library opted in, nothing is built and a pass does nothing: a hold
// received from a sibling is stored by the sync, as every op is, and nothing
// else on the node changes. No row, no replica, no job, no event. An origin
// with it off publishes nothing.
func TestPeerPullOffChangesNothing(t *testing.T) {
	if pp := newPeerPull(config.Defaults(), nil, nil, nil, nil, nil, slog.New(slog.DiscardHandler)); pp != nil {
		t.Fatal("peer-pull was built with no library opted in")
	}

	_, a, b := twoSites(t)
	a.scan(romLibrary)
	a.pass("roms")
	carryOps(t, a, b)

	tables := []string{"blobs", "replicas", "assets", "works", "jobs", "events", "catalog_hold_applications", "catalog_ops"}
	snapshot := func(s *site) map[string]int {
		out := map[string]int{}
		for _, tbl := range tables {
			out[tbl] = s.count(`SELECT count(*) FROM ` + tbl)
		}
		return out
	}
	before := snapshot(b)
	b.pass() // acquisition.peer_pull empty
	if after := snapshot(b); !equalCounts(before, after) {
		t.Fatalf("a pass with peer-pull off changed the node: %v -> %v", before, after)
	}

	_, quiet, _ := twoSites(t)
	quiet.scan(romLibrary)
	quiet.pass()
	if n := quiet.count(`SELECT count(*) FROM catalog_ops`); n != 0 {
		t.Fatalf("an origin with peer-pull off published %d ops", n)
	}
}

// A hold with no opted-in library of its content type here, or over the cap,
// waits with its reason and moves no bytes.
func TestAHoldWaitsWithItsReason(t *testing.T) {
	f, a, b := twoSites(t)
	a.scan(map[string]string{"snes/Super Mario World (USA).sfc": strings.Repeat("smw", 4000)})
	a.pass("roms")
	carryOps(t, a, b)

	b.pass("films") // a name with no library here
	if n := b.count(`SELECT count(*) FROM catalog_hold_applications WHERE outcome = 'no_library'`); n != 1 {
		t.Fatalf("no_library outcomes = %d, want 1", n)
	}
	b.passWith(16, "roms")
	if n := b.count(`SELECT count(*) FROM catalog_hold_applications WHERE outcome = 'too_large'`); n != 1 {
		t.Fatalf("too_large outcomes = %d, want 1", n)
	}
	if n := b.count(`SELECT count(*) FROM jobs WHERE type = ?`, replication.ReplicateBlobJobType); n != 0 {
		t.Fatalf("%d pulls queued for a hold that must wait", n)
	}
	if f.sourceBlobs.requests.Load() != 0 {
		t.Fatal("bytes moved for a hold that must wait")
	}
}

// A pull that failed is not queued again until the spacing has passed.
func TestAFailedPullIsSpaced(t *testing.T) {
	_, a, b := twoSites(t)
	a.scan(map[string]string{"snes/Super Mario World (USA).sfc": strings.Repeat("smw", 4000)})
	a.pass("roms")
	carryOps(t, a, b)
	b.pass("roms")
	blob := strings.Split(a.held()[0], "|")[2]
	self, err := b.cat.SelfPeer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// The pull failed ten minutes ago, and its job is gone.
	b.exec(`DELETE FROM jobs WHERE type = ?`, replication.ReplicateBlobJobType)
	b.exec(`INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at) VALUES (?, ?, 'missing', 0, ?)`,
		blob, self, peerPullNow.Add(-10*time.Minute).Format(time.RFC3339Nano))
	b.pass("roms")
	if n := b.count(`SELECT count(*) FROM jobs WHERE type = ?`, replication.ReplicateBlobJobType); n != 0 {
		t.Fatalf("a pull was queued %d times inside the retry spacing", n)
	}
	var detail string
	if err := b.db.Reader().QueryRow(`SELECT detail FROM catalog_hold_applications`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "retrying after") {
		t.Fatalf("detail = %q, want it to say when the pull is retried", detail)
	}
}

func parseBlob(t *testing.T, s string) hashing.Hash {
	t.Helper()
	h, err := hashing.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
