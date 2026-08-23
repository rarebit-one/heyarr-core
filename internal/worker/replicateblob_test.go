package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// Bytes leaving one machine and arriving, verified, on another (§21, §32,
// ADR-0030, M4-09).
//
// # What the fabric below is, and why it has three nodes rather than two
//
// A destination, a source, and a CONTROLLER that is neither. Two nodes would
// make the source and the controller the same machine, and "the controller
// never carries bytes" would then be unassertable — every byte would come from
// the machine that is also the controller, and any counter would fire either
// way. The third node is what turns §32 from a sentence into a measurement.
//
// # Every refusal here is asserted after the same path has been seen to work
//
// The transfer succeeds first, in TestReplicateBlobTransfersAndVerifies, and
// each refusal below is a refusal of a fabric that has been shown to move bytes
// otherwise. A refusal test on its own passes just as well against a handler
// that refuses everything, which is the failure shape this repository has been
// bitten by repeatedly.

const transferContentSize = 512 << 10

// countingBlobServer is a source's byte-serving surface with a request counter
// and a switch for the two ways a source can misbehave.
//
// It wraps the REAL blobs.Handler rather than reimplementing it: the point of
// these tests is what the destination does with what a source sends, and a
// hand-rolled source would let a mistake in the real one go unnoticed.
type countingBlobServer struct {
	inner *blobs.Handler
	// requests counts every blob-content request that reached this node,
	// whatever it answered. It is the observation the data-path assertion
	// rests on, and it is asserted to FIRE before it is asserted to stay
	// silent.
	requests atomic.Int64
	// wrong, when set, serves these bytes instead, with an ETag naming their
	// digest — a source that honestly labels the wrong content. That is the
	// shape that separates "verify against what the source claimed" from
	// "verify against what we asked for": a source labelling its own wrong
	// bytes passes the first check and fails the second.
	wrong []byte
	// truncateOnce, when set, writes half the body and drops the connection,
	// then clears itself so the retry succeeds.
	truncateOnce atomic.Bool
	// redirectTo, when set, answers 302 to this URL instead of serving. It is
	// the reverse proxy nobody decided to add.
	redirectTo string
	// body is the honest content, used by the truncating path.
	body []byte
	// rangedRequests counts requests carrying a Range header, which is how a
	// test observes that the CHUNKED path ran rather than inferring it from a
	// transfer that succeeded (M5-06).
	rangedRequests atomic.Int64
	// failRangesAfter, when positive, refuses ranged reads once that many have
	// been served. It is how a chunked transfer is interrupted part-way with a
	// real partial left on the destination's disk — a source going away
	// mid-transfer, which is the case resumption exists for.
	failRangesAfter atomic.Int64
}

func (c *countingBlobServer) Content(w http.ResponseWriter, r *http.Request) {
	c.requests.Add(1)
	if r.Header.Get("Range") != "" {
		served := c.rangedRequests.Add(1)
		if limit := c.failRangesAfter.Load(); limit > 0 && served > limit {
			http.Error(w, "this source has gone away mid-transfer", http.StatusServiceUnavailable)
			return
		}
	}
	switch {
	case c.redirectTo != "":
		http.Redirect(w, r, c.redirectTo, http.StatusFound)
	case c.wrong != nil:
		digest, _, err := hashing.HashReader(bytes.NewReader(c.wrong))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// The source says, truthfully, what it is about to send. A destination
		// that believed it would accept these bytes forever.
		w.Header().Set("ETag", `"blake3-`+digest.Hex()+`"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(c.wrong)
	case c.truncateOnce.Load():
		c.truncateOnce.Store(false)
		w.Header().Set("Content-Length", fmt.Sprint(len(c.body)))
		_, _ = w.Write(c.body[:len(c.body)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Abort without finishing the declared body. The destination sees an
		// unexpected EOF, which is what a peer losing power mid-transfer looks
		// like from the other end.
		panic(http.ErrAbortHandler)
	default:
		c.inner.Content(w, r)
	}
}

// transferFabric is a destination, a source and a controller.
type transferFabric struct {
	t *testing.T

	// The destination: the node under test.
	db    *sqlite.DB
	cat   *catalog.Catalog
	store *cas.FS
	self  string

	// The source: a real peer surface over real mTLS.
	sourceID    string
	sourceStore *cas.FS
	sourceSrv   *peerapi.Server
	sourceBlobs *countingBlobServer
	sourceMans  *fabricManifests

	// The controller: a plain client-API blob route, standing in for the node
	// that schedules. It holds the same bytes so that a data-path mistake would
	// SUCCEED rather than fail for an unrelated reason — an assertion that a
	// wrong path also happens not to work proves nothing.
	controller *httptest.Server
	// controllerStore is what the controller's stand-in serves from. A test
	// plants the same bytes there so that a data-path mistake would SUCCEED —
	// an assertion that the wrong path also happens not to work proves nothing.
	controllerStore *cas.FS
	controllerHits  *atomic.Int64

	puller *transfer.Puller
	stamp  string
}

func newTransferFabric(t *testing.T) *transferFabric {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()

	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: "destination", PeerSite: "site-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	self, err := cat.SelfPeer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.OpenFS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}

	f := &transferFabric{
		t: t, db: db, cat: cat, store: store, self: self,
		stamp: time.Now().UTC().Format(time.RFC3339Nano),
	}

	// Identities. Two keypairs, each pinned by the other: there is no CA in
	// this fabric and a certificate is a container for a public key
	// (ADR-0012).
	destPub, destPriv := generateKey(t)
	srcPub, srcPriv := generateKey(t)

	f.sourceID = "01990000-0000-7000-8000-0000000source"
	f.exec(`INSERT INTO peers (id, name, site, mode, is_self, public_key, endpoint, health, created_at, enrolled_at)
		VALUES (?, 'source', 'site-a', 'full', 0, ?, '', 'reachable', ?, ?)`,
		f.sourceID, []byte(srcPub), f.stamp, f.stamp)
	if f.sourceID == f.self {
		t.Fatal("the fixture's source IS the destination; nothing below would prove anything")
	}

	// The source's disk and its peer surface.
	f.sourceStore, err = cas.OpenFS(filepath.Join(dir, "source-cas"))
	if err != nil {
		t.Fatal(err)
	}
	sourceHandler, err := blobs.New(blobs.Options{Store: f.sourceStore})
	if err != nil {
		t.Fatal(err)
	}
	f.sourceBlobs = &countingBlobServer{inner: sourceHandler}
	f.sourceMans = newFabricManifests()
	srcMaterial, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: srcPriv, PeerID: f.sourceID})
	if err != nil {
		t.Fatal(err)
	}
	f.sourceSrv, err = peerapi.New(peerapi.Options{
		Addr:     "127.0.0.1:0",
		Material: srcMaterial,
		// The source admits exactly the destination. Membership is the only
		// trust root in the inter-peer path (ADR-0012).
		Members: mtls.PinnedKey(mtls.Peer{
			PeerID: f.self, Name: "destination", PublicKey: destPub,
		}),
		SelfPeerID: f.sourceID,
		Blobs:      f.sourceBlobs,
		// A manifest route, so a test can put the source into either of §16's
		// two answerable states: it holds a manifest for these bytes, or it
		// holds none and the blob is pulled whole (M5-05, M5-06).
		Manifests: f.sourceMans,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.sourceSrv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.sourceSrv.Shutdown(shutdown)
	})
	f.exec(`UPDATE peers SET endpoint = ? WHERE id = ?`, "https://"+f.sourceSrv.Addr(), f.sourceID)

	// The controller: the client API's blob route, over its own store, behind a
	// counter. Not part of the fabric's membership and never dialled by a
	// transfer — which is the whole assertion.
	var hits atomic.Int64
	f.controllerHits = &hits
	controllerStore, err := cas.OpenFS(filepath.Join(dir, "controller-cas"))
	if err != nil {
		t.Fatal(err)
	}
	controllerHandler, err := blobs.New(blobs.Options{Store: controllerStore})
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/content") {
					hits.Add(1)
				}
				next.ServeHTTP(w, r)
			})
		})
		controllerHandler.Mount(r)
	})
	f.controller = httptest.NewServer(router)
	t.Cleanup(f.controller.Close)
	f.controllerStore = controllerStore

	// The destination's puller, built directly rather than through lazyPuller:
	// these tests are about what a transfer does, and reading a key off disk is
	// what lazyPuller's own test covers.
	destMaterial, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: destPriv, PeerID: f.self})
	if err != nil {
		t.Fatal(err)
	}
	f.puller, err = transfer.New(transfer.Options{Material: destMaterial, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *transferFabric) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Writer().Exec(query, args...); err != nil {
		f.t.Fatalf("seeding (%s): %v", query, err)
	}
}

func generateKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// seedBlob puts content on the SOURCE's disk and on the controller's, tells the
// destination's catalog that the source holds it, and returns the digest.
//
// The controller gets a copy deliberately: a transfer that accidentally read
// from the controller must SUCCEED, so that the only thing distinguishing the
// right path from the wrong one is the counter rather than an error.
func (f *transferFabric) seedBlob(content []byte) hashing.Hash {
	f.t.Helper()
	desc, err := f.sourceStore.Put(f.t.Context(), bytes.NewReader(content))
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.controllerStore.Put(f.t.Context(), bytes.NewReader(content)); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO blobs (hash, size, first_seen_at) VALUES (?, ?, ?)`,
		desc.Hash.String(), desc.Size, f.stamp)
	f.exec(`INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
		VALUES (?, ?, 'present', ?, ?)`, desc.Hash.String(), f.sourceID, desc.Size, f.stamp)
	// The truncating path needs the honest bytes to send half of.
	f.sourceBlobs.body = content
	// The source HOLDS these bytes and has decided nothing about chunking
	// them, which is §16's third state and the state that means "pull whole".
	// A test wanting the chunked path publishes a manifest over the top; a
	// source that answered 404 for the manifest of a blob it holds would be
	// claiming not to have it, which is a different answer and a different
	// action (M5-05).
	f.sourceMans.hold(desc.Hash)
	return desc.Hash
}

// run executes one replicate_blob job for this fabric's destination.
func (f *transferFabric) run(hash hashing.Hash) error {
	f.t.Helper()
	return f.handler()(f.t.Context(), f.job(hash, f.self))
}

func (f *transferFabric) handler() HandlerFunc {
	return ReplicateBlobHandler(TransferDeps{
		Catalog: f.cat,
		Store:   f.store,
		Puller:  func() (*transfer.Puller, error) { return f.puller, nil },
		Logger:  slog.New(slog.DiscardHandler),
	})
}

func (f *transferFabric) job(hash hashing.Hash, destination string) jobs.Job {
	f.t.Helper()
	payload, err := json.Marshal(replication.ReplicateBlobPayload{
		BlobHash: hash.String(), DestinationPeerID: destination,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return jobs.Job{ID: "job-1", Type: replication.ReplicateBlobJobType, Payload: payload}
}

// replicaState is what the destination's catalog says about its own copy.
func (f *transferFabric) replicaState(hash hashing.Hash) (state string, bytesPresent int64, ok bool) {
	f.t.Helper()
	err := f.db.Reader().QueryRow(
		`SELECT state, bytes_present FROM replicas WHERE blob_hash = ? AND peer_id = ?`,
		hash.String(), f.self).Scan(&state, &bytesPresent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return state, bytesPresent, true
}

// transferEvents is every replication.transfer_changed this fabric emitted, in
// order, as (transition, reason) pairs.
func (f *transferFabric) transferEvents(hash hashing.Hash) []string {
	f.t.Helper()
	rows, err := f.db.Reader().Query(
		`SELECT payload FROM events WHERE type = ? AND subject_id = ? ORDER BY seq`,
		events.TypeReplicationTransferChanged, hash.String())
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			f.t.Fatal(err)
		}
		var payload struct {
			Transition string `json:"transition"`
			Reason     string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			f.t.Fatal(err)
		}
		if payload.Reason != "" {
			out = append(out, payload.Transition+":"+payload.Reason)
			continue
		}
		out = append(out, payload.Transition)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// quarantined lists what the destination's store put aside.
func (f *transferFabric) quarantined() []cas.Quarantined {
	f.t.Helper()
	q, err := f.store.QuarantinedBlobs()
	if err != nil {
		f.t.Fatal(err)
	}
	return q
}

// The acceptance condition, and the one every refusal below is measured
// against: a blob present on one peer and absent on the other arrives,
// verifies, and becomes `present`.
func TestReplicateBlobTransfersAndVerifies(t *testing.T) {
	f := newTransferFabric(t)
	content := transferPayload(1)
	hash := f.seedBlob(content)

	if has, err := f.store.Has(t.Context(), hash); err != nil || has {
		t.Fatalf("the destination already holds the blob before the transfer (%v, %v)", has, err)
	}
	if err := f.run(hash); err != nil {
		t.Fatalf("replicate_blob: %v", err)
	}

	// The assertion that matters is about the DESTINATION'S DISK, and it is
	// made by hashing what is on it — not by reading a descriptor, a response
	// header, or the row the handler just wrote. All three of those are things
	// the transfer SAID.
	rsc, _, err := f.store.Open(t.Context(), hash)
	if err != nil {
		t.Fatalf("opening the transferred blob: %v", err)
	}
	defer func() { _ = rsc.Close() }()
	landed, err := io.ReadAll(rsc)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, _, err := hashing.HashReader(bytes.NewReader(landed))
	if err != nil {
		t.Fatal(err)
	}
	if onDisk != hash {
		t.Fatalf("the bytes on the destination's disk hash to %s, want %s", onDisk, hash)
	}
	if !bytes.Equal(landed, content) {
		t.Fatal("the bytes on the destination's disk are not the bytes the source holds")
	}

	state, bytesPresent, ok := f.replicaState(hash)
	if !ok {
		t.Fatal("the transfer left no replicas row at all")
	}
	assertEq(t, state, "present", "the replica state after a verified transfer")
	if bytesPresent != int64(len(content)) {
		t.Fatalf("bytes_present = %d, want %d", bytesPresent, len(content))
	}

	// The bytes came off the source, over the peer surface, once.
	if got := f.sourceBlobs.requests.Load(); got != 1 {
		t.Fatalf("the source served %d blob-content requests, want exactly 1", got)
	}
	if q := f.quarantined(); len(q) != 0 {
		t.Fatalf("a successful transfer quarantined %d blobs", len(q))
	}

	// One event type carrying the transition, started then succeeded — not one
	// type per edge (events.go, M4-15).
	assertTransitions(t, f.transferEvents(hash), []string{
		replication.TransferStarted, replication.TransferSucceeded,
	})
}

// Invariant 9. The queue WILL re-run this.
func TestReplicateBlobRunTwiceIsOneReplica(t *testing.T) {
	f := newTransferFabric(t)
	hash := f.seedBlob(transferPayload(2))

	if err := f.run(hash); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	served := f.sourceBlobs.requests.Load()
	if err := f.run(hash); err != nil {
		t.Fatalf("the second run returned an error, so the handler is not idempotent: %v", err)
	}

	state, _, ok := f.replicaState(hash)
	if !ok {
		t.Fatal("no replicas row after two runs")
	}
	assertEq(t, state, "present", "the replica state after a re-run")

	var rows int
	if err := f.db.Reader().QueryRow(
		`SELECT count(*) FROM replicas WHERE blob_hash = ? AND peer_id = ?`,
		hash.String(), f.self).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	assertEq(t, fmt.Sprint(rows), "1", "the number of replicas rows after two runs")

	// The second run moved no bytes: it found them already held and said so
	// without asking anybody.
	if got := f.sourceBlobs.requests.Load(); got != served {
		t.Fatalf("the second run made the source serve %d more requests; a re-run must not re-transfer",
			got-served)
	}
	// And it announced nothing. A re-run that changes nothing must also SAY
	// nothing, or every retry becomes event noise (the rule inventory
	// reconciliation follows).
	assertTransitions(t, f.transferEvents(hash), []string{
		replication.TransferStarted, replication.TransferSucceeded,
	})
}

// A source serving the wrong bytes for a hash, reproduced only after the same
// fabric has been seen to transfer honestly.
func TestReplicateBlobRefusesBytesThatAreNotWhatWasAskedFor(t *testing.T) {
	f := newTransferFabric(t)

	// First: the transfer works. Without this the refusal below proves nothing.
	honest := f.seedBlob(transferPayload(3))
	if err := f.run(honest); err != nil {
		t.Fatalf("the honest transfer failed, so nothing below is a refusal of anything: %v", err)
	}
	if state, _, _ := f.replicaState(honest); state != "present" {
		t.Fatalf("the honest transfer left state %q, want present", state)
	}

	// Then: corrupt the source. It serves different bytes AND labels them
	// honestly, which is what separates verifying against the claim from
	// verifying against the expectation.
	wanted := f.seedBlob(transferPayload(4))
	f.sourceBlobs.wrong = transferPayload(99)

	err := f.run(wanted)
	if err == nil {
		t.Fatal("the destination accepted bytes that are not the bytes it asked for")
	}
	var corrupt *cas.Corruption
	if !errors.As(err, &corrupt) {
		t.Fatalf("error = %v, want a *cas.Corruption naming both digests", err)
	}
	if corrupt.Hash != wanted {
		t.Fatalf("the corruption names %s as expected, want %s", corrupt.Hash, wanted)
	}

	// No replica, no present row.
	if has, err := f.store.Has(t.Context(), wanted); err != nil || has {
		t.Fatalf("Has(%s) = %v, %v — wrong bytes became a blob", wanted, has, err)
	}
	state, _, ok := f.replicaState(wanted)
	if !ok {
		t.Fatal("the refused transfer left no replicas row; the fabric would look as if it had never tried")
	}
	assertEq(t, state, "corrupt", "the replica state after a source served wrong bytes")

	// And a quarantine entry: a source that sent wrong bytes is evidence worth
	// keeping (ADR-0018).
	q := f.quarantined()
	if len(q) != 1 {
		t.Fatalf("quarantine holds %d entries, want exactly 1", len(q))
	}
	assertEq(t, q[0].Hash.String(), wanted.String(),
		"the quarantined blob is filed under the digest that was expected")

	assertTransitions(t, f.transferEvents(wanted), []string{
		replication.TransferStarted, replication.TransferFailed + ":verification_failed",
	})
}

// §32, measured rather than inspected.
//
// The observation is proven to work FIRST — a genuine controller-mediated read
// makes the counter move — and only then is it asserted to stay still. An
// assertion on an absence that has never been seen to fire is worth nothing.
func TestReplicateBlobKeepsTheControllerOutOfTheDataPath(t *testing.T) {
	f := newTransferFabric(t)
	hash := f.seedBlob(transferPayload(5))

	// Phase 1: prove the counter fires. This is an ordinary client-API blob
	// read against the controller, of the very bytes the transfer is about to
	// move, and it is the same route a redirect would land on.
	resp, err := f.controller.Client().Get(
		f.controller.URL + "/api/v1/blobs/" + hash.String() + "/content")
	if err != nil {
		t.Fatalf("the controller-mediated read failed, so the observation is untested: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	assertEq(t, fmt.Sprint(resp.StatusCode), "200", "the controller-mediated read's status")
	if int64(len(body)) == 0 {
		t.Fatal("the controller served no bytes, so it is not a working data path to be excluded from")
	}
	baseline := f.controllerHits.Load()
	if baseline != 1 {
		t.Fatalf("the controller's counter reads %d after one genuine read; it is not measuring anything",
			baseline)
	}

	// Phase 2: the transfer. The controller holds these exact bytes and would
	// have served them happily — so if the counter moves, it moved because the
	// destination asked it to.
	if err := f.run(hash); err != nil {
		t.Fatalf("replicate_blob: %v", err)
	}
	if got := f.controllerHits.Load(); got != baseline {
		t.Fatalf("the controller served %d blob-content requests during the transfer; "+
			"the controller must never be in the data path (§32, ADR-0030)", got-baseline)
	}
	if got := f.sourceBlobs.requests.Load(); got != 1 {
		t.Fatalf("the source served %d blob-content requests, want exactly 1 — "+
			"if this is 0 the bytes came from somewhere else entirely", got)
	}

	// Phase 3: the way the controller actually ends up in the data path, which
	// is never a decision anybody records. A source fronted by something that
	// answers 302 — a reverse proxy for an awkward NAT, a migration, a load
	// balancer — and a client that helpfully follows it.
	redirected := f.seedBlob(transferPayload(6))
	f.sourceBlobs.redirectTo = f.controller.URL + "/api/v1/blobs/" + redirected.String() + "/content"

	err = f.run(redirected)
	// The COUNTER is checked first, deliberately. It is the mechanical
	// assertion — the one that measures where the bytes went rather than what
	// the code decided to return — and checking the error first would make a
	// data-path violation report itself as a missing error value.
	if got := f.controllerHits.Load(); got != baseline {
		t.Fatalf("the controller served %d blob-content requests after the redirect; "+
			"following it is exactly how controller availability becomes playback availability (§53)",
			got-baseline)
	}
	if err == nil {
		t.Fatal("the destination followed a redirect out of the peer fabric")
	}
	if !errors.Is(err, transfer.ErrRedirected) {
		t.Fatalf("error = %v, want ErrRedirected", err)
	}
	if has, err := f.store.Has(t.Context(), redirected); err != nil || has {
		t.Fatalf("Has(%s) = %v, %v — a redirected pull produced a blob", redirected, has, err)
	}
}

// A transfer that dies mid-flight, and the retry that then succeeds.
func TestReplicateBlobInterruptedLeavesNoReplicaAndRetries(t *testing.T) {
	f := newTransferFabric(t)
	hash := f.seedBlob(transferPayload(7))
	f.sourceBlobs.truncateOnce.Store(true)

	if err := f.run(hash); err == nil {
		t.Fatal("an interrupted transfer reported success")
	}

	// No present row, and nothing partially readable. This is the assertion the
	// `present`-before-verification sabotage breaks.
	state, bytesPresent, ok := f.replicaState(hash)
	if !ok {
		t.Fatal("the interrupted transfer left no replicas row")
	}
	if state == "present" {
		t.Fatal("a transfer that never finished left a `present` row — the catalog now claims a " +
			"replica the disk does not have, and garbage collection reads that table (ADR-0018)")
	}
	assertEq(t, state, "missing", "the replica state after an interrupted transfer")
	assertEq(t, fmt.Sprint(bytesPresent), "0", "bytes_present after an interrupted transfer")
	if has, err := f.store.Has(t.Context(), hash); err != nil || has {
		t.Fatalf("Has(%s) = %v, %v — half a transfer became a readable blob", hash, has, err)
	}
	if _, _, err := f.store.Open(t.Context(), hash); !errors.Is(err, cas.ErrNotFound) {
		t.Fatalf("Open after an interrupted transfer = %v, want ErrNotFound", err)
	}

	// Retried whole. Resumption is Milestone 5 (§84); this is what makes the
	// handler idempotent rather than merely re-runnable.
	if err := f.run(hash); err != nil {
		t.Fatalf("the retry after an interrupted transfer: %v", err)
	}
	state, bytesPresent, _ = f.replicaState(hash)
	assertEq(t, state, "present", "the replica state after the retry")
	if bytesPresent != int64(transferContentSize) {
		t.Fatalf("bytes_present = %d after the retry, want %d", bytesPresent, transferContentSize)
	}

	assertTransitions(t, f.transferEvents(hash), []string{
		replication.TransferStarted, replication.TransferFailed + ":no_source_delivered",
		replication.TransferStarted, replication.TransferSucceeded,
	})
}

// Revocation is the deletion of a membership record (ADR-0012). A peer that is
// no longer a member is no longer a source, and the refusal happens before a
// connection exists.
func TestReplicateBlobRefusesASourceThatIsNotAMember(t *testing.T) {
	f := newTransferFabric(t)

	// First, the same fabric transferring successfully.
	first := f.seedBlob(transferPayload(8))
	if err := f.run(first); err != nil {
		t.Fatalf("the transfer before revocation failed, so nothing below is a refusal: %v", err)
	}
	served := f.sourceBlobs.requests.Load()
	if served != 1 {
		t.Fatalf("the source served %d requests for the first transfer, want 1", served)
	}

	// Then revoke: remove the membership record, which is the entire mechanism
	// — there is no CRL to consult (ADR-0012).
	wanted := f.seedBlob(transferPayload(9))
	f.exec(`DELETE FROM peers WHERE id = ?`, f.sourceID)

	err := f.run(wanted)
	if !errors.Is(err, replication.ErrNoSource) {
		t.Fatalf("error = %v, want ErrNoSource — a revoked peer must not be a source", err)
	}
	if got := f.sourceBlobs.requests.Load(); got != served {
		t.Fatalf("the revoked source served %d further requests; the refusal must happen before any "+
			"bytes move", got-served)
	}
	if has, err := f.store.Has(t.Context(), wanted); err != nil || has {
		t.Fatalf("Has(%s) = %v, %v — bytes arrived from a peer that is not a member", wanted, has, err)
	}
	if _, _, ok := f.replicaState(wanted); ok {
		t.Fatal("a transfer that was refused before it started wrote a replicas row; nothing about " +
			"this peer's disk changed and the row would be a fact nobody established")
	}
	if len(f.transferEvents(wanted)) != 0 {
		t.Fatal("a transfer that never started emitted a lifecycle event")
	}
}

// A candidate with no pinned key is refused without a connection, and it is
// refused by name: dialling the endpoint and accepting whatever answered would
// be trust on first use with extra steps (ADR-0012).
func TestReplicateBlobRefusesASourceWithNothingToPin(t *testing.T) {
	f := newTransferFabric(t)
	hash := f.seedBlob(transferPayload(10))
	f.exec(`UPDATE peers SET public_key = NULL WHERE id = ?`, f.sourceID)

	err := f.run(hash)
	if !errors.Is(err, replication.ErrNoSource) {
		t.Fatalf("error = %v, want ErrNoSource", err)
	}
	if got := f.sourceBlobs.requests.Load(); got != 0 {
		t.Fatalf("the source was dialled %d times despite having no pinned key", got)
	}

	// And the domain says why, addressably, rather than only by disappearing
	// from a list.
	unpinnable := replication.Source{PeerID: "p", Endpoint: "https://example:8443"}
	if err := unpinnable.Usable(); !errors.Is(err, replication.ErrSourceNotPinnable) {
		t.Fatalf("Usable() = %v, want ErrSourceNotPinnable", err)
	}
}

// A destination pulls its own bytes (ADR-0030). A job naming another peer is
// work for that peer's queue, and this node must not quietly complete it.
func TestReplicateBlobRefusesAJobForAnotherPeer(t *testing.T) {
	f := newTransferFabric(t)
	hash := f.seedBlob(transferPayload(11))

	err := f.handler()(t.Context(), f.job(hash, f.sourceID))
	if err == nil {
		t.Fatal("this node accepted a transfer destined for another peer")
	}
	if !strings.Contains(err.Error(), f.sourceID) {
		t.Fatalf("error = %v, want it to name the destination it was offered", err)
	}
	if has, err := f.store.Has(t.Context(), hash); err != nil || has {
		t.Fatalf("Has(%s) = %v, %v — a job for another peer pulled bytes onto this one", hash, has, err)
	}
	if got := f.sourceBlobs.requests.Load(); got != 0 {
		t.Fatalf("a job for another peer made %d requests to the source", got)
	}
}

// The registration is a value, so the two properties it IS can be asserted.
func TestReplicateBlobRegistrationIsBoundedAndUnconditional(t *testing.T) {
	reg := ReplicateBlobRegistration(TransferDeps{})
	if reg.MaxConcurrent <= 0 {
		t.Fatal("replicate_blob is registered unbounded; a first full sync would run one transfer " +
			"per queue slot against one disk (runtime.go:33)")
	}
	if reg.MaxConcurrent != maxConcurrentTransfers {
		t.Fatalf("MaxConcurrent = %d, want %d", reg.MaxConcurrent, maxConcurrentTransfers)
	}
	assertEq(t, reg.RequiredCapability, "",
		"replicate_blob's required capability: pulling bytes needs a network and a disk, nothing else")
}

// Health ranks candidates and does not exclude them — see RankSources.
func TestRankSourcesPrefersReachableWithoutExcludingUnknown(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	candidates := []replication.Source{
		{PeerID: "c", Endpoint: "https://c:1", PublicKey: key, Health: replication.HealthUnreachable},
		{PeerID: "b", Endpoint: "https://b:1", PublicKey: key, Health: replication.HealthUnknown},
		{PeerID: "a", Endpoint: "https://a:1", PublicKey: key, Health: replication.HealthReachable},
		{PeerID: "d", Endpoint: "https://d:1", Health: replication.HealthReachable},
	}
	got := replication.RankSources(candidates)
	var order []string
	for _, s := range got {
		order = append(order, s.PeerID)
	}
	assertEq(t, strings.Join(order, ","), "a,b,c",
		"the source order: reachable, then unknown, then unreachable, and never a peer with no key")
}

// transferPayload is deterministic content of a size worth streaming.
//
// Large enough that the body does not fit in one buffer, which is what makes
// the truncation test a truncation rather than a refusal to start, and what
// exercises the streaming verification rather than a single Write.
func transferPayload(seed byte) []byte {
	out := make([]byte, transferContentSize)
	for i := range out {
		out[i] = seed + byte(i%251)
	}
	return out
}

func assertEq(t *testing.T, got, want, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

func assertTransitions(t *testing.T, got, want []string) {
	t.Helper()
	assertEq(t, strings.Join(got, " → "), strings.Join(want, " → "),
		"the replication.transfer_changed transitions")
}
