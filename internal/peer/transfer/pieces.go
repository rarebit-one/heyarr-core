package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	domaintransfer "github.com/rarebit-one/heyarr-core/internal/domain/transfer"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/pieces"
)

// ErrNoPieceSource means no source offered a piece this node still needs.
//
// Distinct from "the transfer failed": it is the answer when every source is
// reachable and simply does not have the rest yet, which in a swarm where two
// peers are both still fetching is an ordinary and temporary state (§23). The
// caller retries later rather than treating the blob as unobtainable.
var ErrNoPieceSource = errors.New("transfer: no source holds a piece this node still needs")

// pieceProgressStore is the staging-side capability a piece pull needs beyond
// [Store]. Asserted at the call site rather than added to Store, because every
// other implementation and test double would otherwise grow methods only M6
// uses — the same reasoning the controller's peer surface applies.
type pieceProgressStore interface {
	SavePieceProgress(blob hashing.Hash, encoded string) error
	LoadPieceProgress(blob hashing.Hash) (string, error)
	DiscardPieceProgress(blob hashing.Hash) error
}

// PieceOutcome is what one piece pull did.
//
// The per-source counters are the number §23 exists to produce. A pull that
// took every piece from one peer and a pull that spread across four both end in
// a published blob, and an operator who cannot tell them apart cannot tell
// whether the swarm is doing anything.
type PieceOutcome struct {
	// Bytes is the published blob's length, verified whole.
	Bytes int64
	// PieceLength is the division this transfer used.
	PieceLength int64
	// Fetched is how many pieces this attempt pulled over the network.
	Fetched int
	// Resumed is how many a previous attempt had already landed, taken from
	// the bitset and NOT refetched.
	Resumed int
	// FromPeer counts pieces served, by peer id. This is also the record a
	// retry uses to prefer peers that did not contribute to a failed attempt —
	// a heuristic and not attribution, per ADR-0043.
	FromPeer map[string]int
	// Unreachable names the sources this session could not use — one that
	// never said what it held, or one that failed every piece it was asked
	// for and delivered none.
	//
	// It is reported rather than returned as an error, which is ADR-0041's
	// rule made visible: a session makes progress with whoever it has, so an
	// unreachable participant is a fact about the run and never its verdict.
	// Sorted, so a test can assert on it.
	Unreachable []string
	// Deduplicated reports the store already held the blob by the time this
	// finished. Success, not conflict.
	Deduplicated bool
}

// PullPieces fetches a blob as fixed-length pieces from many sources at once.
//
// # What this is for
//
// Replication moves a blob from ONE source that holds all of it. That shape
// cannot express two peers who each hold a third of the same blob and are both
// still fetching — the case §23 is about — because until now a peer holding
// part of a blob was indistinguishable from one holding none.
//
// # Why the size is a parameter and not something this discovers
//
// The geometry is derived from the blob's size, and both ends must derive the
// SAME one or they are talking about different byte ranges under the same index
// (ADR-0043). This node knows the size because it decided to fetch the blob —
// from an inventory entry or a catalogue row that stated it. A source that
// reports a different size is describing a different blob and is dropped, which
// is why the size arrives from the caller rather than from whichever peer
// answered first.
//
// # What it trusts
//
// Nothing. A source's availability is a claim, a piece served is bytes, and the
// only statement about them not made by the peer serving them is the blob's own
// BLAKE3 digest — which is checked, whole, at Publish (invariant 1). There are
// no piece hashes and adding them would not help; ADR-0043 says why.
func (p *Puller) PullPieces(
	ctx context.Context, blob hashing.Hash, size int64, sources []Candidate,
) (PieceOutcome, error) {
	g, err := pieces.For(size)
	if err != nil {
		return PieceOutcome{}, fmt.Errorf("transfer: dividing %s: %w", blob, err)
	}
	out := PieceOutcome{PieceLength: g.PieceLength, FromPeer: map[string]int{}}

	// Already here? A concurrent transfer or an ingest won the race, which is
	// success and not a conflict — the same answer Pull gives.
	if held, herr := p.store.Has(ctx, blob); herr == nil && held {
		out.Deduplicated = true
		out.Bytes = size
		return out, nil
	}

	partial, err := p.store.OpenPartial(ctx, blob)
	if err != nil {
		return out, fmt.Errorf("transfer: staging %s: %w", blob, err)
	}
	defer func() { _ = partial.Close() }()

	have := p.resume(blob, g)
	out.Resumed = have.Count()

	// Ask everyone what they hold before fetching anything. A source that
	// cannot be asked is dropped rather than fatal: §23's rule is that a
	// session makes progress with whoever it has.
	//
	// This survey is the session's FIRST question, not its only one. The other
	// peers are filling up while this one is, so a source that answers "nothing"
	// now is asked again as the transfer progresses — see [Puller.resurvey].
	held, silent := p.survey(ctx, blob, g, sources)
	if len(held) == 0 && !g.Complete(have) {
		out.Unreachable = silent
		return out, ErrNoPieceSource
	}

	s := newPieceSession(g, have, partial)
	for _, id := range silent {
		s.silent[id] = struct{}{}
	}

	// Every source at once, bounded, each fetching the rarest piece it holds.
	// The order is only the order workers pick sources up in, so it is sorted
	// for determinism rather than because anything depends on it.
	order := make([]string, 0, len(held))
	for id := range held {
		order = append(order, id)
	}
	sort.Strings(order)
	p.driveSources(ctx, s, blob, held, order)

	out.Fetched = s.fetched
	out.FromPeer = s.fromPeer
	out.Unreachable = s.unusable()
	p.saveProgress(blob, g, s.have)

	if s.err != nil {
		return out, s.err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if !g.Complete(s.have) {
		// Everything still missing is held by nobody who answered. What landed
		// is saved above, so the next attempt resumes from it.
		return out, ErrNoPieceSource
	}

	desc, err := partial.Publish(ctx, blob)
	if err != nil {
		// The whole-object check is the only verification there is, so a
		// failure here means one of the contributors sent bad bytes and there
		// is no way to say which — every one of them is equally a suspect
		// (ADR-0043). Drop the staging file and the bitset: resuming from a
		// record that produced a bad blob would reproduce it.
		p.discardProgress(blob)
		_ = partial.Discard()
		return out, fmt.Errorf(
			"transfer: %s did not verify after %d pieces from %d peers, so one of them "+
				"served bytes it could not back and there is no way to say which: %w",
			blob, out.Fetched, len(out.FromPeer), err)
	}
	out.Bytes = desc.Size
	p.discardProgress(blob)

	p.log.Info("a blob was assembled from pieces",
		"blob", blob.String(), "bytes", out.Bytes, "pieces_fetched", out.Fetched,
		"pieces_resumed", out.Resumed, "peers", len(out.FromPeer),
		"unreachable", len(out.Unreachable))
	return out, nil
}

// sourceClaim is one source, what it says it holds, and what it has already
// failed to deliver.
type sourceClaim struct {
	src   replication.Source
	kind  domaintransfer.Kind
	claim pieces.Availability
	// declined is every piece this source was asked for and did not deliver:
	// refused, served at the wrong length, or unreachable at that moment.
	//
	// It is separate from the claim, and outlives a re-survey, because a
	// re-survey replaces the claim wholesale with whatever the peer now says —
	// which for a peer that holds the blob is "everything", including the piece
	// it just failed. Without this the session asks the same source for the
	// same piece forever, which it did: the symptom was one piece fetched
	// hundreds of times until the machine ran out of ephemeral ports.
	declined map[int]struct{}
}

// declines records that this source did not deliver a piece it claimed.
func (sc *sourceClaim) declines(index int) {
	if sc.declined == nil {
		sc.declined = map[int]struct{}{}
	}
	sc.declined[index] = struct{}{}
}

// resume reads the bitset a previous attempt left, as a HINT about where to
// start. A record that cannot be read or does not describe this geometry is
// simply an empty one: the cost of being wrong is refetching, and the cost of
// believing a stale record is a blob that does not verify.
func (p *Puller) resume(blob hashing.Hash, g pieces.Geometry) pieces.Availability {
	empty := pieces.NewAvailability(g.Count())
	store, ok := p.store.(pieceProgressStore)
	if !ok {
		return empty
	}
	encoded, err := store.LoadPieceProgress(blob)
	if err != nil || encoded == "" {
		return empty
	}
	was, resumed, err := pieces.Decode(encoded)
	if err != nil || was != g {
		// A record from a different division of a different size. Start over
		// rather than reinterpret its bits under this geometry.
		return empty
	}
	return resumed
}

func (p *Puller) saveProgress(blob hashing.Hash, g pieces.Geometry, have pieces.Availability) {
	store, ok := p.store.(pieceProgressStore)
	if !ok {
		return
	}
	if err := store.SavePieceProgress(blob, pieces.Encode(g, have)); err != nil {
		// Not fatal. The bitset is an optimisation; losing it costs a refetch
		// on the next attempt and nothing else.
		p.log.Debug("could not record piece progress",
			"blob", blob.String(), "error", err)
	}
}

func (p *Puller) discardProgress(blob hashing.Hash) {
	if store, ok := p.store.(pieceProgressStore); ok {
		_ = store.DiscardPieceProgress(blob)
	}
}

// survey asks every source what it holds, dropping those that cannot answer or
// that describe a different geometry.
//
// A source describing a different SIZE is describing a different blob. It is
// dropped and named in the log rather than failing the transfer, because one
// confused peer must not stop a swarm that has others.
func (p *Puller) survey(
	ctx context.Context, blob hashing.Hash, g pieces.Geometry, sources []Candidate,
) (map[string]*sourceClaim, []string) {
	out := map[string]*sourceClaim{}
	silent := map[string]struct{}{}
	for _, candidate := range sources {
		src := candidate.Source
		if err := src.Usable(); err != nil {
			silent[src.PeerID] = struct{}{}
			continue
		}

		// A web seed is not ASKED what it holds. It serves byte ranges of a
		// whole blob or it does not have the blob at all, so "which pieces" has
		// no meaning for it and there is no route to ask on. It claims
		// everything, and a 404 on the first range is how it says otherwise —
		// which the fetch loop already handles as one source failing one piece.
		if candidate.Kind == domaintransfer.KindWebSeed {
			all := pieces.NewAvailability(g.Count())
			for i := range g.Count() {
				all.Add(i)
			}
			out[src.PeerID] = &sourceClaim{src: src, kind: candidate.Kind, claim: all}
			continue
		}
		if candidate.Kind != domaintransfer.KindPeer {
			p.log.Warn("a source of a kind this transport cannot drive was skipped",
				"blob", blob.String(), "peer_id", src.PeerID, "kind", string(candidate.Kind),
				"error", ErrUndrivableSource)
			continue
		}

		encoded, err := p.fetchAvailability(ctx, src, blob)
		if err != nil {
			p.log.Debug("a source could not say what it holds",
				"blob", blob.String(), "peer_id", src.PeerID, "error", err)
			silent[src.PeerID] = struct{}{}
			continue
		}
		if encoded == "" {
			continue
		}
		theirs, claim, derr := pieces.Decode(encoded)
		if derr != nil {
			p.log.Debug("a source's availability could not be read",
				"blob", blob.String(), "peer_id", src.PeerID, "error", derr)
			continue
		}
		if theirs != g {
			p.log.Warn("a source divides this blob differently, so it is not a source for it",
				"blob", blob.String(), "peer_id", src.PeerID,
				"their_size", theirs.Size, "our_size", g.Size)
			continue
		}
		out[src.PeerID] = &sourceClaim{src: src, kind: candidate.Kind, claim: claim}
	}
	return out, sortedKeys(silent)
}

// fetchAvailability asks one source which pieces it holds.
func (p *Puller) fetchAvailability(
	ctx context.Context, src replication.Source, blob hashing.Hash,
) (string, error) {
	client, err := p.clientFor(src)
	if err != nil {
		return "", err
	}
	origin, err := p.originFor(src)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		origin+peerapi.PieceAvailabilityPath(blob.String()), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", p.refusal(resp, src, blob)
	}

	var body peerapi.PieceAvailabilityResponse
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxAvailabilityBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return "", fmt.Errorf("peer %s answered 200 with something that is not an "+
			"availability: %w", src.PeerID, err)
	}
	return body.Available, nil
}

// maxAvailabilityBody bounds the availability read. A bitset for the largest
// blob the geometry produces is a few kilobytes of hex; this is generous by
// three orders of magnitude and still not "whatever the peer sends".
const maxAvailabilityBody = 1 << 20

// fetchPiece pulls one piece's bytes from one source.
func (p *Puller) fetchPiece(
	ctx context.Context, src replication.Source, blob hashing.Hash, index int,
) ([]byte, error) {
	client, err := p.clientFor(src)
	if err != nil {
		return nil, err
	}
	origin, err := p.originFor(src)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		origin+peerapi.PiecePath(blob.String(), index), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, p.refusal(resp, src, blob)
	}
	// Bounded by the piece length the geometry implies, so a peer cannot make
	// this node allocate by answering with a stream that does not end. The
	// caller checks the length it got against the length it wanted.
	return io.ReadAll(io.LimitReader(resp.Body, maxPieceRead))
}

// maxPieceRead bounds one piece read. One byte past the largest piece the
// geometry can produce, so a peer serving exactly a maximum piece succeeds and
// one serving more is caught by the length check rather than truncated into
// looking correct.
const maxPieceRead = int64(pieces.MaxPieceLength) + 1
