package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"

	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
)

// 🔴 THE assertion for §27: a blob is completed from web seeds ALONE, with no
// piece exchange anywhere in the run.
//
// This is the sharp one because it proves the web seed is a real source rather
// than an optimisation that only works alongside peers. The sources here serve
// the ordinary blob content route and nothing else — they have no piece source
// behind their peer surface at all, so every piece route on them refuses.
func TestABlobIsCompletedFromWebSeedsAlone(t *testing.T) {
	content := pieceFixture(t, 8)
	blob := digestOfBytes(t, content)

	one := newNode(t, "peer-seed-one", "seed one")
	two := newNode(t, "peer-seed-two", "seed two")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(one.member(), two.member(), dst.member())

	// startSource mounts the blob routes and NO piece source. That is what a
	// web seed is: a member with the bytes that does not run piece exchange.
	s1 := startSource(t, one, root, content)
	s2 := startSource(t, two, root, content)

	d := newDestination(t, dst)
	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{
			transfer.WebSeed(sourceFor(one, s1.addr)),
			transfer.WebSeed(sourceFor(two, s2.addr)),
		})
	if err != nil {
		t.Fatalf("completing a blob from web seeds alone: %v", err)
	}
	if out.Fetched != 8 {
		t.Errorf("fetched %d pieces, want 8", out.Fetched)
	}
	if err := d.store.Verify(t.Context(), blob); err != nil {
		t.Errorf("the assembled blob does not verify: %v", err)
	}

	got, _, err := d.store.Open(t.Context(), blob)
	if err != nil {
		t.Fatalf("the blob is not in the store: %v", err)
	}
	defer func() { _ = got.Close() }()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Error("the bytes assembled from web seeds are not the fixture's bytes")
	}
}

// A mixed session: some pieces from a peer, some from a web seed, and the
// ATTRIBUTION asserted for both.
//
// "It completed" is satisfied by a session that used only one of them, which is
// why the counts are the assertion rather than the outcome.
func TestAMixedSessionTakesPiecesFromBothKindsAndSaysWhichCameFromWhere(t *testing.T) {
	content := pieceFixture(t, 8)
	blob := digestOfBytes(t, content)

	peer := newNode(t, "peer-exchanger", "exchanger")
	seed := newNode(t, "peer-seed", "seed")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(peer.member(), seed.member(), dst.member())

	// The peer holds an interleaved HALF and speaks piece exchange. The web
	// seed has everything but no piece routes. Interleaved rather than split,
	// so a puller that took a prefix from one and a suffix from the other
	// cannot pass by coincidence.
	holder := newPieceHolder(t, content, 0, 2, 4, 6)
	p := startPieceSource(t, peer, root, holder)
	s := startSource(t, seed, root, content)

	d := newDestination(t, dst)
	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{
			transfer.Peer(sourceFor(peer, p.addr)),
			transfer.WebSeed(sourceFor(seed, s.addr)),
		})
	if err != nil {
		t.Fatalf("mixed session: %v", err)
	}
	if err := d.store.Verify(t.Context(), blob); err != nil {
		t.Fatalf("the assembled blob does not verify: %v", err)
	}

	fromPeer := out.FromPeer[peer.peerID]
	fromSeed := out.FromPeer[seed.peerID]
	if fromPeer == 0 {
		t.Error("no piece came from the peer, so piece exchange contributed nothing")
	}
	if fromSeed == 0 {
		t.Error("no piece came from the web seed, so the web seed contributed nothing")
	}
	if fromPeer+fromSeed != 8 {
		t.Errorf("attribution accounts for %d of 8 pieces (%d peer, %d seed)",
			fromPeer+fromSeed, fromPeer, fromSeed)
	}
	// The four the peer alone holds are rarest, so they come from the peer;
	// the other four are held only by the seed.
	if fromPeer != 4 || fromSeed != 4 {
		t.Errorf("pieces split %d peer / %d seed, want 4 and 4 — the peer holds "+
			"0,2,4,6 and only the seed has the rest", fromPeer, fromSeed)
	}
}

// A web seed that serves bytes it cannot back is caught by the whole-object
// check, exactly like a lying peer — there is nothing special about a web seed
// on the trust side (ADR-0036, and ADR-0043 on why there is no earlier check).
func TestAWebSeedServingWrongBytesIsCaughtByTheWholeObjectCheck(t *testing.T) {
	content := pieceFixture(t, 4)
	// The digest of the RIGHT bytes; the seed serves different ones.
	blob := digestOfBytes(t, content)
	wrong := append([]byte(nil), content...)
	wrong[len(wrong)/2] ^= 0xFF

	seed := newNode(t, "peer-liar-seed", "liar seed")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(seed.member(), dst.member())

	s := startSource(t, seed, root, wrong)
	d := newDestination(t, dst)

	_, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{transfer.WebSeed(sourceFor(seed, s.addr))})
	if err == nil {
		t.Fatal("a blob assembled from a web seed's wrong bytes was accepted")
	}
	if held, _ := d.store.Has(t.Context(), blob); held {
		t.Error("the store holds a blob that did not verify")
	}
}

// 🔴 The SERVING side gained nothing, which is what §27 and ADR-0013 ask for.
//
// A fixed piece IS a byte range, so a web seed needs no piece awareness — and
// this asserts that none crept in. If serving ever DOES need to change, that is
// a finding worth arguing rather than a diff that slips through.
//
// Parsed rather than grepped: a comment mentioning pieces is fine, an
// identifier or import is not.
func TestTheBlobServingPackageHasNoPieceAwareness(t *testing.T) {
	dir := filepath.Join("..", "..", "api", "blobs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	var offences []string
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		seen++
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly|parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, "storagefabric/pieces") || strings.Contains(path, "torrent") {
				offences = append(offences, name+" imports "+path)
			}
		}

		full, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		ast.Inspect(full, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			lower := strings.ToLower(id.Name)
			for _, bad := range []string{"piecelength", "pieceindex", "piececount", "torrent"} {
				if strings.Contains(lower, bad) {
					offences = append(offences, name+" declares or uses "+id.Name)
				}
			}
			return true
		})
	}

	if seen == 0 {
		t.Fatalf("no non-test Go files found in %s, so this test asserts nothing", dir)
	}
	if len(offences) > 0 {
		t.Errorf("the blob serving package has piece awareness, which §27 says it should not "+
			"need — a piece is a byte range and the range machinery already existed:\n  %s",
			strings.Join(offences, "\n  "))
	}
}

// A web seed is used only when it is DECLARED one. The kind is never inferred,
// because a peer whose piece route is broken must look broken rather than
// quietly downgrade to ranged reads.
func TestAPeerIsNotSilentlyTreatedAsAWebSeed(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	seed := newNode(t, "peer-seed", "seed")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(seed.member(), dst.member())

	// It has the bytes and serves ranges, but it is declared a PEER — so the
	// transport asks its piece routes, which are not mounted, and gets nothing.
	s := startSource(t, seed, root, content)
	d := newDestination(t, dst)

	_, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{transfer.Peer(sourceFor(seed, s.addr))})
	if err == nil {
		t.Fatal("a source declared a peer completed a transfer through the web-seed path, " +
			"so the kind is being inferred rather than believed")
	}
	if held, _ := d.store.Has(t.Context(), blob); held {
		t.Error("the blob landed anyway")
	}
}

var _ = mtls.PinnedKey

// misbehavingSeed is a web seed that answers ranged reads WRONGLY, in the two
// ways §27 names: ignoring the Range header, and serving more than was asked
// for.
//
// It exists because the honest fixture cannot express either. A source that
// always behaves proves the happy path and leaves every refusal unproven —
// which is exactly what sabotage found: deleting the 206 check and deleting the
// over-long check both left the suite green.
type misbehavingSeed struct {
	content []byte
	// mode selects the misbehaviour:
	//   "ignore-range"  answer 200 with the WHOLE blob
	//   "overlong"      answer 206 with one byte too many
	//   "wrong-status"  answer 200 with EXACTLY the right bytes
	//
	// The third exists because the first two do not isolate the 206 check.
	// A whole-blob 200 is caught by the over-long guard, so deleting the status
	// check left the suite green — a test passing for a reason it did not
	// claim. Right bytes, wrong status is the case where ONLY the status check
	// can refuse.
	mode string
}

func (m *misbehavingSeed) Content(w http.ResponseWriter, r *http.Request) {
	spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
	fromText, toText, ok := strings.Cut(spec, "-")
	if !ok {
		http.Error(w, "the fixture wants a range", http.StatusBadRequest)
		return
	}
	from, _ := strconv.ParseInt(fromText, 10, 64)
	to, _ := strconv.ParseInt(toText, 10, 64)
	if from < 0 || to < from || to >= int64(len(m.content)) {
		http.Error(w, "out of range", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	switch m.mode {
	case "ignore-range":
		// The whole blob, with 200. A destination that does not INSIST on 206
		// writes the blob's first bytes wherever the piece was meant to go —
		// and for piece 0 that even looks correct.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(m.content)
	case "overlong":
		// 206, the right offset, one byte too many. The surplus byte is the
		// blob's first rather than the one after the range, so a range ending
		// at the blob's end can be lengthened too.
		body := append(append([]byte(nil), m.content[from:to+1]...), m.content[0])
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", from, to, len(m.content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	case "wrong-status":
		// Exactly the bytes asked for, announced as 200 rather than 206. The
		// content is correct, so no length check and no hash can object; the
		// only thing wrong is that the source did not say it was serving a
		// range, which means it did not agree it was asked for one.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(m.content[from : to+1])
	default:
		http.Error(w, "unknown fixture mode", http.StatusInternalServerError)
	}
}

func startMisbehavingSeed(
	t *testing.T, self *node, members mtls.Membership, seed *misbehavingSeed,
) *sourceNode {
	t.Helper()
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		Blobs:      seed,
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutting a misbehaving seed down: %v", err)
		}
	})
	return &sourceNode{self: self, addr: srv.Addr()}
}

// 🔴 A web seed that IGNORES the Range header is refused, not assembled.
//
// This is the failure that would be silent. A source ignoring Range answers the
// WHOLE blob with 200, and its first bytes are a perfectly plausible piece 0 —
// so a destination that accepts any 2xx assembles a blob out of the same prefix
// repeated, and only the final whole-object hash notices, with nothing to say
// about which source was at fault.
//
// Requiring 206 is what makes that impossible rather than merely unlikely.
func TestAWebSeedThatIgnoresTheRangeHeaderIsRefused(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	seed := newNode(t, "peer-sloppy-seed", "sloppy seed")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(seed.member(), dst.member())

	s := startMisbehavingSeed(t, seed, root, &misbehavingSeed{content: content, mode: "ignore-range"})
	d := newDestination(t, dst)

	_, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{transfer.WebSeed(sourceFor(seed, s.addr))})
	if err == nil {
		t.Fatal("a web seed that ignored Range completed a transfer")
	}
	if !errors.Is(err, transfer.ErrNoPieceSource) {
		t.Errorf("err = %v, want ErrNoPieceSource — every piece was refused, so no "+
			"source held anything usable", err)
	}
	if held, _ := d.store.Has(t.Context(), blob); held {
		t.Error("a blob was published from a source that ignored the range it was asked for")
	}
}

// 🔴 A web seed that serves MORE than the range asked for is refused.
//
// An over-long 206 is a source answering a question it was not asked, and the
// surplus bytes are exactly what the next piece's offset would land on. Written
// at the piece's offset, the extra byte corrupts a range belonging to a
// DIFFERENT piece — which is why the length is checked before the write and not
// after.
func TestAWebSeedThatServesMoreThanTheRangeIsRefused(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	seed := newNode(t, "peer-greedy-seed", "greedy seed")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(seed.member(), dst.member())

	s := startMisbehavingSeed(t, seed, root, &misbehavingSeed{content: content, mode: "overlong"})
	d := newDestination(t, dst)

	_, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{transfer.WebSeed(sourceFor(seed, s.addr))})
	if err == nil {
		t.Fatal("a web seed that served more than it was asked for completed a transfer")
	}
	if held, _ := d.store.Has(t.Context(), blob); held {
		t.Error("a blob was published from over-long responses")
	}
}

// 🔴 A web seed answering 200 instead of 206 is refused EVEN WHEN the bytes are
// right.
//
// This isolates the status check, and it exists because the other two
// misbehaviour tests do not. A source that ignores Range answers the whole blob,
// which the over-long guard catches — so deleting the 206 requirement left both
// of them green. Sabotage found that, and this is the case where only the
// status check can refuse: correct bytes, correct length, wrong status.
//
// Why refuse at all when the bytes are right: 200 means the source did not
// agree it was serving a range. The next request to it is a different range,
// and a source that quietly ignores the header will answer that one with the
// whole blob too. Accepting the first is how a transfer starts trusting a
// source that is not honouring its side of the contract.
func TestAWebSeedAnsweringTwoHundredIsRefusedEvenWithTheRightBytes(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	seed := newNode(t, "peer-status-seed", "status seed")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(seed.member(), dst.member())

	s := startMisbehavingSeed(t, seed, root, &misbehavingSeed{content: content, mode: "wrong-status"})
	d := newDestination(t, dst)

	_, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{transfer.WebSeed(sourceFor(seed, s.addr))})
	if err == nil {
		t.Fatal("a web seed answering 200 completed a transfer, so the 206 requirement " +
			"is not being enforced — the bytes happened to be right this time")
	}
	if held, _ := d.store.Has(t.Context(), blob); held {
		t.Error("the blob was published from responses that never claimed to be ranged")
	}
}
