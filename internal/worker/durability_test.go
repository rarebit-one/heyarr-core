package worker

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// ADR-0018's placement precondition, against the real schema (M4-12).
//
// The unit tests in internal/storagefabric/integrity pin the decision. These
// pin the half only a database can be wrong about: that `replicas` really does
// cascade away underneath the delete, that the evidence written first really
// does survive it, and that the seam refuses a reclaim nothing established.

// addPeer inserts another peer and returns its id.
func (h *harness) addPeer(name, endpoint, health string) string {
	h.t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	if _, err := h.db.Writer().ExecContext(h.t.Context(), `
		INSERT INTO peers (id, name, site, mode, public_key, endpoint, is_self, health, created_at)
		VALUES (?, ?, '', 'full', ?, ?, 0, ?, ?)`,
		id, name, []byte("pinned-"+name), endpoint, health,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// claimReplica gives a peer a `replicas` row for a blob.
func (h *harness) claimReplica(hash, peerID, state string, reportedAt time.Time) {
	h.t.Helper()
	var reported any
	if !reportedAt.IsZero() {
		reported = reportedAt.UTC().Format(time.RFC3339Nano)
	}
	if _, err := h.db.Writer().ExecContext(h.t.Context(), `
		INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, reported_at, updated_at)
		VALUES (?, ?, ?, 1, ?, ?)
		ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
			state = excluded.state, reported_at = excluded.reported_at`,
		hash, peerID, state, reported, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		h.t.Fatal(err)
	}
}

// The finding that started this issue, asserted rather than described.
//
// replicas.blob_hash is ON DELETE CASCADE, so Catalog.Reclaim's DELETE takes
// the record of who else held the blob with it: the transaction that decides
// deleting is safe destroys the evidence it decided on. Evidence written FIRST,
// into a table with no foreign key to `blobs` (00028), is what survives — and
// "on what grounds?" is only ever asked after the blob is gone.
func TestDurabilityEvidenceOutlivesTheBlobRowAndTheReplicasThatCascadeWithIt(t *testing.T) {
	h := newHarness(t)
	h.write("Evidenced Film (2019)/Evidenced Film (2019).mkv", "bytes with a witness")
	res := h.ingest("Evidenced Film (2019)/Evidenced Film (2019).mkv")
	hash := hashing.MustParse(res.BlobHash)
	peerID := h.addPeer("site-b", "https://site-b.example:8443", "reachable")
	h.claimReplica(res.BlobHash, peerID, "present", time.Now().UTC())

	if got := h.count("replicas"); got < 2 {
		t.Fatalf("replicas = %d, want the self row and site-b's", got)
	}

	// The order the collector uses: evidence, then the delete.
	if err := h.catalog.RecordDurability(t.Context(), integrity.Evidence{
		BlobHash: hash, Size: res.BlobSize, Basis: integrity.BasisVerifiedRemote,
		PeerID: peerID, PeerName: "site-b", Endpoint: "https://site-b.example:8443",
		ReportedAt: time.Now().UTC(), RecordedAt: time.Now().UTC(),
		Detail: "site-b answered that it holds these bytes",
	}); err != nil {
		t.Fatal(err)
	}
	h.deleteAsset(h.assetOf(res.BlobHash))
	if err := h.catalog.Reclaim(t.Context(), hash, res.BlobSize, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// The cascade happened. This is not incidental — it is the reason the
	// evidence table exists, so it is asserted rather than assumed.
	if got := h.count("blobs"); got != 0 {
		t.Fatalf("blobs = %d, want the row gone", got)
	}
	if got := h.count("replicas"); got != 0 {
		t.Fatalf("replicas = %d, want them cascaded away with the blob — if this is non-zero the "+
			"schema changed and this test is no longer measuring the hazard", got)
	}

	// And the evidence is still readable, naming the peer that no longer has a
	// replicas row here.
	ev, err := h.catalog.DurabilityEvidence(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 {
		t.Fatalf("durability evidence = %+v, want one row surviving the delete", ev)
	}
	if ev[0].PeerID != peerID || ev[0].Basis != integrity.BasisVerifiedRemote {
		t.Errorf("evidence = %+v, want it to name %s on the %q basis",
			ev[0], peerID, integrity.BasisVerifiedRemote)
	}
	if ev[0].ReportedAt.IsZero() {
		t.Error("the evidence lost the freshness it was established on, which is the half that " +
			"makes it arguable with later")
	}
}

// The defence-in-depth check at the reclaim seam, mirroring ON DELETE RESTRICT.
//
// A collector that lost the precondition to a refactor still cannot unlink a
// tracked blob, because the database layer refuses a delete nothing established.
// Watched declining, because a backstop nobody has seen catch anything is a
// comment.
func TestTheCatalogRefusesToReclaimABlobNothingEstablishedTheDurabilityOf(t *testing.T) {
	h := newHarness(t)
	h.write("Unwitnessed Film (2020)/Unwitnessed Film (2020).mkv", "bytes with no witness")
	res := h.ingest("Unwitnessed Film (2020)/Unwitnessed Film (2020).mkv")
	h.deleteAsset(h.assetOf(res.BlobHash))

	err := h.catalog.Reclaim(t.Context(), hashing.MustParse(res.BlobHash), res.BlobSize, true,
		time.Now().UTC())
	if err == nil {
		t.Fatal("the catalog deleted a blob with no durability evidence behind it")
	}
	if got := h.count("blobs"); got != 1 {
		t.Errorf("blobs = %d, want the blob to have survived", got)
	}

	// Untracked bytes are exempt by construction: they have no blobs row, so
	// they are nobody's replica and there is no placement policy for them to
	// satisfy. Asserted so the guard cannot quietly swallow M1-10's cleanup.
	if err := h.catalog.Reclaim(t.Context(), hashing.MustParse(res.BlobHash), res.BlobSize, false,
		time.Now().UTC()); err != nil {
		t.Errorf("the guard refused an UNTRACKED reclaim, which has no durability question: %v", err)
	}
}

// A lying row is corrected where the catalog can see it, and the correction is
// an event like every other state transition (invariant 7).
func TestCorrectingALyingReplicaRowMovesItToMissingAndSaysSo(t *testing.T) {
	h := newHarness(t)
	h.write("Contradicted Film (2021)/Contradicted Film (2021).mkv", "bytes a peer denies holding")
	res := h.ingest("Contradicted Film (2021)/Contradicted Film (2021).mkv")
	peerID := h.addPeer("site-b", "https://site-b.example:8443", "reachable")
	h.claimReplica(res.BlobHash, peerID, "present", time.Now().UTC())
	hash := hashing.MustParse(res.BlobHash)

	if err := h.catalog.MarkReplicaMissing(t.Context(), hash, peerID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	replicas, err := h.catalog.Replicas(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(replicas) != 1 || replicas[0].State != "missing" {
		t.Fatalf("replicas = %+v, want site-b's row corrected to missing", replicas)
	}
	if replicas[0].Present() {
		t.Error("a corrected row still reads as present")
	}
	evs := h.eventsOfType(events.TypeReplicaMissing)
	if len(evs) != 1 {
		t.Fatalf("emitted %d %s events, want 1", len(evs), events.TypeReplicaMissing)
	}

	// Idempotent: a second correction is silence, not a second loss.
	if err := h.catalog.MarkReplicaMissing(t.Context(), hash, peerID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got := len(h.eventsOfType(events.TypeReplicaMissing)); got != 1 {
		t.Errorf("emitted %d %s events after a repeat correction, want 1 (invariant 9)",
			got, events.TypeReplicaMissing)
	}
}

// "Is this blob somewhere ELSE" must never be answered by a row describing this
// machine. The filter is in the SQL rather than in every caller, so it is
// asserted where it lives.
func TestThePeerViewsNeverIncludeThisNode(t *testing.T) {
	h := newHarness(t)
	h.write("Local Film (2022)/Local Film (2022).mkv", "bytes only this node holds")
	res := h.ingest("Local Film (2022)/Local Film (2022).mkv")
	hash := hashing.MustParse(res.BlobHash)

	// Ingest already wrote a `present` replicas row for the self peer.
	if state, _ := h.replicaState(res.BlobHash); state != "present" {
		t.Fatalf("self replica state = %q, want present — this fixture is meant to have one", state)
	}

	peers, err := h.catalog.Peers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 {
		t.Fatalf("Peers = %+v on a single-peer deployment, want none", peers)
	}
	replicas, err := h.catalog.Replicas(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(replicas) != 0 {
		t.Fatalf("Replicas = %+v, want none — the self row must not answer \"is it elsewhere\"", replicas)
	}

	// With a second peer, both views answer, and reported_at comes back as the
	// zero time when nobody has ever confirmed the row (00023 leaves it NULL
	// on purpose).
	peerID := h.addPeer("site-b", "https://site-b.example:8443", "unreachable")
	h.claimReplica(res.BlobHash, peerID, "present", time.Time{})

	peers, err = h.catalog.Peers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].PeerID != peerID {
		t.Fatalf("Peers = %+v, want just site-b", peers)
	}
	if peers[0].Reachable() {
		t.Error("a peer stored as unreachable reads as reachable")
	}
	replicas, err = h.catalog.Replicas(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(replicas) != 1 {
		t.Fatalf("Replicas = %+v, want just site-b's", replicas)
	}
	if !replicas[0].ReportedAt.IsZero() {
		t.Errorf("reported_at = %s, want the zero time for a row no peer ever confirmed",
			replicas[0].ReportedAt)
	}
	if replicas[0].Fresh(time.Now().UTC(), integrity.DefaultFreshness) {
		t.Error("a row no peer has ever confirmed reads as fresh, which is how a decade-old " +
			"belief becomes a reason to delete")
	}
	if len(replicas[0].Peer.PublicKey) == 0 {
		t.Error("the replica came back with no pinned key, so nothing that peer said could be " +
			"attributed to it (ADR-0012)")
	}
}

// A basis this system does not establish anything on is refused where it can be
// explained, rather than by a CHECK constraint that names neither the blob nor
// the caller.
func TestAnUnrecognisedDurabilityBasisIsRefused(t *testing.T) {
	h := newHarness(t)
	h.write("Basis Film (2023)/Basis Film (2023).mkv", "bytes with a made-up justification")
	res := h.ingest("Basis Film (2023)/Basis Film (2023).mkv")

	err := h.catalog.RecordDurability(t.Context(), integrity.Evidence{
		BlobHash: hashing.MustParse(res.BlobHash), Size: res.BlobSize,
		Basis: "the_replicas_row_said_so", RecordedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("the catalog accepted evidence on a basis nothing establishes")
	}
	if got := h.count("durability_evidence"); got != 0 {
		t.Errorf("durability_evidence = %d, want 0", got)
	}
}

// assetOf finds the asset row pointing at a blob, so a test can dereference it.
func (h *harness) assetOf(hash string) string {
	h.t.Helper()
	var id string
	if err := h.db.Reader().QueryRowContext(h.t.Context(),
		`SELECT id FROM assets WHERE blob_hash = ? LIMIT 1`, hash).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}
