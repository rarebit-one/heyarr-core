package transfer

import (
	"context"
	"fmt"
	"sort"
	"sync"

	domaintransfer "github.com/rarebit-one/heyarr-core/internal/domain/transfer"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/pieces"
)

// maxConcurrentSources bounds how many sources one session drives at once.
//
// A session that opens a connection per known peer is a thundering herd on a
// large fabric, and the fabric this targets is small enough that the bound is
// never reached in practice — which is the point. The number is here so that
// the behaviour at fifty peers is a decision rather than an accident.
//
// Sources beyond the bound are not dropped. They are simply not driven
// concurrently: a worker that finishes takes the next source that still has
// something this node needs, so the set in flight changes over the session
// while the count does not.
const maxConcurrentSources = 8

// pieceSession is the state one piece pull shares between the goroutines
// driving its sources.
//
// # Why a session object rather than channels
//
// Every decision this transport makes is a decision about SHARED state — which
// pieces are still missing, which are already being fetched by somebody else,
// and which sources still claim something worth asking for. A channel of work
// items would have to be recomputed and refilled every time a source's claim
// changed, and a source's claim changes constantly: §23's whole premise is that
// the other peers are filling up at the same time this one is.
//
// So the queue is derived, never stored. A worker asks "what should I fetch
// next, given everything known right now", under a lock, and the answer is
// correct at the instant it is given.
//
// # What the lock covers, and what it deliberately does not
//
// It covers the bitset, the in-flight set, the claims and the counters — and
// the staging file's writes. It does NOT cover the network fetch, which is the
// slow half and the whole reason for the concurrency.
//
// Putting WriteAt inside the lock is deliberate and is not an oversight to be
// optimised away later. [cas.Partial] is a single-owner handle: its Size is a
// plain field, and two goroutines writing disjoint ranges through it would
// still race on that field. Serialising the local write — microseconds — to
// parallelise the network read — the entire cost — keeps Partial's contract
// intact without a mutex inside every implementation of it.
type pieceSession struct {
	mu   sync.Mutex
	cond *sync.Cond

	g       pieces.Geometry
	have    pieces.Availability
	partial cas.Partial

	// inflight is the pieces some worker is fetching right now. It is what
	// stops two sources being asked for the same piece — division, not a race.
	inflight map[int]struct{}
	// fetching is how many workers are inside a network fetch. A worker with
	// nothing to do may only give up when this is zero: while anything is in
	// flight, more work can still appear.
	fetching int
	// generation advances whenever a piece lands. An idle source re-asks what
	// its peer holds once per generation and no more, which is what turns a
	// one-shot survey into a swarm without turning it into a poll.
	generation uint64
	// stopped is set when the context is cancelled, so a worker parked on the
	// condition variable wakes rather than waiting for a piece that will never
	// arrive.
	stopped bool
	// live is how many workers are still driving a source, and idle how many
	// of them currently have nothing to fetch.
	//
	// The pair exists for one case that is easy to get wrong and hard to see:
	// a source that holds NOTHING when the session opens. It has no work, so it
	// wants to give up — but at that instant the other workers may not have
	// started their first fetch yet, so "nothing is in flight" is true and
	// means the opposite of what it looks like. A worker may only conclude the
	// session is stuck when EVERY live worker agrees it is stuck and nothing is
	// in flight. Otherwise a peer that was empty at the survey and fills up a
	// moment later — §23's ordinary case — is dropped before it can help.
	live int
	idle int

	// err is a fault on this node that ended the session, as opposed to a
	// source that could not help.
	err error

	fetched  int
	fromPeer map[string]int
	// failed counts refusals per source. A source that failed and never
	// delivered anything is what the outcome calls unreachable; one that
	// failed a piece and delivered ten is simply a peer having a bad moment,
	// and calling it unreachable would be a worse lie than saying nothing.
	failed map[string]int
	// silent is a source that could not say what it holds at all.
	silent map[string]struct{}

	// roster is this node's membership the last time it was re-read, and
	// rosterAt the generation that read reflects. A session re-reads membership
	// at most once per generation — the re-survey's cadence, not a query per
	// piece — so a peer revoked mid-transfer is dropped within one generation
	// rather than at the next session (#290). nil means "not read yet, or the
	// Puller has no membership source", in which case no source is dropped for
	// revocation, which is the pre-#290 behaviour.
	roster   map[string]bool
	rosterAt uint64
}

func newPieceSession(g pieces.Geometry, have pieces.Availability, partial cas.Partial) *pieceSession {
	s := &pieceSession{
		g: g, have: have, partial: partial,
		inflight: map[int]struct{}{},
		fromPeer: map[string]int{},
		failed:   map[string]int{},
		silent:   map[string]struct{}{},
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// watch wakes every parked worker when the context ends.
//
// Without it a cancelled session would sit on the condition variable until a
// fetch that is itself cancelled happened to complete. The returned function
// stops the watcher, and must be called or the goroutine outlives the session.
func (s *pieceSession) watch(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.stopped = true
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// rarityLocked is how many surveyed sources claim a piece.
func rarityLocked(index int, held map[string]*sourceClaim) int {
	n := 0
	for _, h := range held {
		if h.claim.Has(index) {
			n++
		}
	}
	return n
}

// assignLocked picks the piece this source should fetch next, or reports that
// it has nothing to offer right now.
//
// Rarest-first, as the sequential driver was: the piece the fewest sources hold
// is the piece that disappears if one of them goes away, and a swarm that
// fetches common pieces first ends with everybody waiting on the same last
// peer. What is new is that the choice is made from the pieces THIS source
// claims, so two sources holding different halves are both busy rather than
// queued behind one another.
//
// Ties break by lowest index, which makes a single-source pull deterministic
// and therefore assertable. With several sources the interleaving is not
// deterministic and must not be asserted on; what is assertable is that every
// source contributed, which is the claim §23 actually makes.
func (s *pieceSession) assignLocked(sc *sourceClaim, held map[string]*sourceClaim) (int, bool) {
	best, bestRarity := -1, 0
	for _, index := range s.have.Missing() {
		if index >= s.g.Count() {
			break
		}
		if _, busy := s.inflight[index]; busy {
			continue
		}
		if !sc.claim.Has(index) {
			continue
		}
		if _, no := sc.declined[index]; no {
			continue
		}
		if r := rarityLocked(index, held); best < 0 || r < bestRarity {
			best, bestRarity = index, r
		}
	}
	if best < 0 {
		return 0, false
	}
	s.inflight[best] = struct{}{}
	s.fetching++
	return best, true
}

// landed records a piece whose bytes are on disk.
//
// The bitset is written AFTER the bytes, never before — a crash between the two
// costs a refetch, and the other order would have this node serving a piece it
// does not have (ADR-0043). Concurrency does not weaken that: a worker adds its
// own index only after its own WriteAt returned, so a bitset saved from another
// goroutine in between can never claim a piece whose bytes have not landed.
func (s *pieceSession) landed(index int, peerID string) {
	delete(s.inflight, index)
	s.fetching--
	s.have.Add(index)
	s.fetched++
	s.fromPeer[peerID]++
	s.generation++
	s.cond.Broadcast()
}

// refused puts a piece back and forgets that this source claimed it.
//
// One source failing one piece is not the transfer failing. The piece returns
// to the pool for somebody else, which is what the issue means by
// redistribution rather than skipping: a source that dies half way through has
// its outstanding work picked up, not abandoned.
func (s *pieceSession) refused(index int, sc *sourceClaim) {
	delete(s.inflight, index)
	s.fetching--
	sc.claim.Remove(index)
	sc.declines(index)
	s.generation++
	s.cond.Broadcast()
}

// revoke drops a source whose membership was withdrawn mid-session (#290).
//
// Revocation is the deletion of a membership record (ADR-0012), and ADR-0038's
// rule is that a member going away is an ordinary day: the session must stop
// asking it for pieces and keep going with whoever remains. So its claim is
// cleared — nothing counts it as a holder any longer, and rarity stops
// weighting pieces by a source that will never serve them — and its worker
// returns, which is where its outstanding work is picked up. Every piece it had
// not yet served is still MISSING, and every source that also claims those
// pieces will now be assigned them: the same redistribution #280 built for a
// source that dies mid-transfer, reached here without a second path.
//
// Bytes it already served are KEPT. The whole object is verified at Publish
// against a digest that did not come from this peer (invariant 1, ADR-0043), so
// a revoked contributor cannot have made the blob wrong, and discarding its
// pieces would be a cost with no benefit.
func (s *pieceSession) revoke(sc *sourceClaim) {
	sc.claim = pieces.NewAvailability(s.g.Count())
	// Advance the generation and wake every parked worker: a source leaving the
	// pool changes what the others should do next, exactly as a piece landing
	// does, and an idle worker holding out for this source must re-evaluate.
	s.generation++
	s.cond.Broadcast()
}

// abort ends the session because of a fault on THIS node.
//
// A source that will not serve a piece is somebody else's problem and is
// handled by asking somebody else. A staging file this node cannot write is
// not: no other source can fix it, every retry fails the same way, and the
// piece would come back round on the next re-survey. It ends the session and
// the caller reports why.
func (s *pieceSession) abort(err error) {
	if s.err == nil {
		s.err = err
	}
	s.stopped = true
	s.cond.Broadcast()
}

// sortedKeys is the deterministic order an outcome reports sources in.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// driveSources runs every usable source at once, bounded, until the blob is
// complete or nothing can make further progress.
//
// # Completion is the digest, and never the participants
//
// ADR-0041: a session makes progress with whoever it has. Nothing here waits
// for a quorum, nothing blocks on an unreachable source, and the loop's exit
// condition is the bitset — not a count of workers, not a count of peers, and
// not an error from any one of them. A source that cannot be reached at all is
// recorded in the outcome and otherwise has no effect on it, which is the
// difference between a swarm and a barrier.
func (p *Puller) driveSources(
	ctx context.Context, s *pieceSession, blob hashing.Hash,
	held map[string]*sourceClaim, order []string,
) {
	stop := s.watch(ctx)
	defer stop()

	workers := min(len(order), maxConcurrentSources)
	next := 0
	var wg sync.WaitGroup
	var pick sync.Mutex

	s.mu.Lock()
	s.live = workers
	s.mu.Unlock()

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A worker between two sources is still live: it is about to take
			// the next one. It stops being live only when there are none left.
			defer func() {
				s.mu.Lock()
				s.live--
				s.cond.Broadcast()
				s.mu.Unlock()
			}()
			for {
				pick.Lock()
				if next >= len(order) {
					pick.Unlock()
					return
				}
				sc := held[order[next]]
				next++
				pick.Unlock()
				if sc == nil {
					continue
				}
				p.driveOneSource(ctx, s, blob, sc, held)
			}
		}()
	}
	wg.Wait()
}

// stillMember reports whether a source is still in this node's membership.
//
// # Why per generation and not per piece
//
// Re-reading membership for every piece would be a query per request. Re-reading
// it once per generation — the cadence the re-survey already runs at — bounds a
// revoked source's exposure at one generation while costing one membership read
// per generation shared across every worker, which is nearly free next to the
// network round trips a generation already involves (#290). A worker driving a
// busy source calls this each iteration, but the read behind it happens once per
// generation; the rest are cache hits.
//
// # Fail safe, not open
//
// A Puller with no membership source (Options.Members unset) does not enforce
// revocation mid-session and every source stays — the pre-#290 behaviour, and
// safe because refusing to SERVE a revoked peer is mTLS's job at the connection
// (ADR-0012), not this session's. A membership read that ERRORS keeps the last
// roster this session held rather than dropping every source at once: a
// transient catalog error is not evidence that the whole fabric was revoked, and
// treating it as such would abandon a transfer that ADR-0038 says should make
// progress with whoever it has. Until the FIRST read succeeds the roster is nil
// and no source is dropped.
func (p *Puller) stillMember(ctx context.Context, s *pieceSession, peerID string) bool {
	if p.members == nil {
		return true
	}

	s.mu.Lock()
	gen := s.generation
	roster, fresh := s.roster, s.rosterAt == gen && s.roster != nil
	s.mu.Unlock()

	if !fresh {
		read, err := p.members(ctx)
		if err != nil {
			p.log.Debug("could not re-read membership mid-session; keeping the current roster",
				"peer_id", peerID, "error", err)
		} else {
			s.mu.Lock()
			// Record against the generation observed at read time. A later
			// generation simply re-reads next time round, which is correct: the
			// roster is a hint that costs a refetch of who is present, never a
			// wrong byte.
			s.roster, s.rosterAt = read, gen
			roster = read
			s.mu.Unlock()
		}
	}

	// nil roster: not read yet, or every read has failed. Do not drop.
	if roster == nil {
		return true
	}
	return roster[peerID]
}

// driveOneSource fetches from one source until it has nothing left to give.
//
// # Why a source goroutine rather than a piece goroutine
//
// A worker per PIECE would have to choose a source for each piece and would
// therefore need a load-balancing rule — which is a heuristic, and one that
// would have to be re-tuned every time a source's speed changed. A worker per
// SOURCE needs none: a source that answers quickly takes more pieces because it
// comes back for more sooner, and a source that stalls holds up exactly one
// piece. The division follows the throughput instead of predicting it.
func (p *Puller) driveOneSource(
	ctx context.Context, s *pieceSession, blob hashing.Hash,
	sc *sourceClaim, held map[string]*sourceClaim,
) {
	// The initial survey counts as generation zero's, so a source does not
	// re-ask before anything has changed.
	var surveyedAt uint64

	for {
		// Re-read membership before fetching the next piece, at most once per
		// generation (#290). A source revoked since the survey is dropped here
		// rather than at the next session: it stops being asked, and returning
		// hands its outstanding work to the sources that remain. This is the
		// one check that must run even while a source is BUSY — a peer that
		// claims the whole blob never enters the idle re-survey branch below,
		// so a membership check gated on that branch would never fire for the
		// case the issue is about.
		if !p.stillMember(ctx, s, sc.src.PeerID) {
			p.log.Info("a source was revoked mid-transfer, so it stops receiving pieces",
				"blob", blob.String(), "peer_id", sc.src.PeerID)
			s.mu.Lock()
			s.revoke(sc)
			s.mu.Unlock()
			return
		}

		s.mu.Lock()
		var index int
		var ok bool
		for {
			if s.stopped || s.g.Complete(s.have) {
				s.mu.Unlock()
				return
			}
			if index, ok = s.assignLocked(sc, held); ok {
				break
			}
			// Nothing this source can offer against what is missing right now.
			//
			// That is not the end of it. The other peers in this swarm are
			// filling up at the same time (§23), so a source with nothing to
			// give at generation N may have something at generation N+1 — but
			// only if somebody asks it again. A one-shot survey is why a peer
			// that started empty stays empty forever from here, and why two
			// peers starting together would never exchange anything at all.
			if sc.kind == domaintransfer.KindPeer && surveyedAt < s.generation {
				at := s.generation
				s.mu.Unlock()
				p.resurvey(ctx, blob, s, sc)
				s.mu.Lock()
				surveyedAt = at
				continue
			}
			// Surveyed at this generation and still nothing.
			s.idle++
			if s.idle >= s.live && s.fetching == 0 {
				// Every live worker is out of ideas and nothing is in flight,
				// so no piece can arrive to change any of their minds. The
				// session is finished with what it has — which is not the same
				// as failed, and the caller decides that by looking at the
				// bitset rather than at this.
				s.idle--
				s.cond.Broadcast()
				s.mu.Unlock()
				return
			}
			s.cond.Wait()
			s.idle--
		}
		s.mu.Unlock()

		body, ferr := p.fetchFrom(ctx, sc, blob, s.g, index)
		if ferr != nil {
			p.log.Debug("a piece could not be fetched",
				"blob", blob.String(), "piece", index, "peer_id", sc.src.PeerID, "error", ferr)
			s.mu.Lock()
			s.refused(index, sc)
			s.failed[sc.src.PeerID]++
			s.mu.Unlock()
			continue
		}

		off, length, rerr := s.g.Range(index)
		if rerr != nil {
			s.mu.Lock()
			s.refused(index, sc)
			s.mu.Unlock()
			continue
		}
		// A short or long piece is a source disagreeing about the geometry
		// after all. Refuse the bytes rather than write them at an offset they
		// do not belong at — this is the one write that could corrupt a range
		// some other piece owns.
		if int64(len(body)) != length {
			p.log.Debug("a peer served a piece of the wrong length",
				"blob", blob.String(), "piece", index, "peer_id", sc.src.PeerID,
				"got", len(body), "want", length)
			s.mu.Lock()
			s.refused(index, sc)
			s.mu.Unlock()
			continue
		}

		s.mu.Lock()
		if _, werr := s.partial.WriteAt(body, off); werr != nil {
			// This node's fault, not the source's, so it is not handled by
			// asking somebody else — it ends the session.
			p.log.Warn("a piece could not be staged",
				"blob", blob.String(), "piece", index, "error", werr)
			delete(s.inflight, index)
			s.fetching--
			s.abort(fmt.Errorf("transfer: writing piece %d of %s: %w", index, blob, werr))
			s.mu.Unlock()
			return
		}
		s.landed(index, sc.src.PeerID)
		p.saveProgress(blob, s.g, s.have)
		s.mu.Unlock()
	}
}

// fetchFrom asks one source for one piece, by whichever contract it speaks.
func (p *Puller) fetchFrom(
	ctx context.Context, sc *sourceClaim, blob hashing.Hash, g pieces.Geometry, index int,
) ([]byte, error) {
	if sc.kind == domaintransfer.KindWebSeed {
		// A web seed has no piece route. A piece is a byte range, so it is a
		// ranged GET on the ordinary content route (§27, ADR-0013), and the
		// serving side needs no piece awareness at all.
		return p.fetchPieceFromWebSeed(ctx, sc.src, blob, g, index)
	}
	return p.fetchPiece(ctx, sc.src, blob, index)
}

// resurvey re-asks one peer what it holds, and merges the answer.
//
// A web seed is never re-surveyed: it has no availability route and claims
// everything by construction, so there is nothing about it that can change.
//
// An answer that cannot be read, or that describes a different geometry, leaves
// the existing claim alone rather than clearing it. A momentary refusal is not
// evidence that a peer has stopped holding what it held a second ago, and
// treating it as such would make a swarm forget its members under load.
func (p *Puller) resurvey(
	ctx context.Context, blob hashing.Hash, s *pieceSession, sc *sourceClaim,
) {
	encoded, err := p.fetchAvailability(ctx, sc.src, blob)
	if err != nil || encoded == "" {
		if err != nil {
			p.log.Debug("a source could not say what it holds",
				"blob", blob.String(), "peer_id", sc.src.PeerID, "error", err)
			s.mu.Lock()
			s.silent[sc.src.PeerID] = struct{}{}
			s.mu.Unlock()
		}
		return
	}
	theirs, claim, derr := pieces.Decode(encoded)
	if derr != nil || theirs != s.g {
		return
	}
	s.mu.Lock()
	sc.claim = claim
	// A source that has just told us it holds something may have unblocked a
	// worker that gave up on it, so the wake is not optional.
	s.cond.Broadcast()
	s.mu.Unlock()
}

// unusable is the sources this session could not use, sorted.
//
// A source that failed and delivered nothing, or that never said what it held
// at all. A source that failed one piece and delivered ten is NOT here: it is a
// peer that had a bad moment, and naming it unreachable would be a worse
// statement than making none.
func (s *pieceSession) unusable() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]struct{}{}
	for id := range s.silent {
		out[id] = struct{}{}
	}
	for id, n := range s.failed {
		if n > 0 && s.fromPeer[id] == 0 {
			out[id] = struct{}{}
		}
	}
	return sortedKeys(out)
}
