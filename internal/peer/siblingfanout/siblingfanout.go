// Package siblingfanout is how an origin learns what its siblings did with an
// acquire op it published (ADR-0119 #924 amendment).
//
// A sibling's outcome for an `acquire` (applied, held, deferred, ...) is
// recorded only at that sibling. The origin asks each sibling over its peer
// surface, `GET /peer/v1/fan-out?op=<hash>`, and the sibling answers for the
// ops the asking certificate's key signed. Nothing is written at either end:
// the answer is for display in the origin's acquisition status, and nothing
// decides anything from it.
//
// # Never in the way of the status call
//
// Each sibling is asked concurrently under a short timeout, and every answer,
// including "unreachable", is cached for a TTL. A status read therefore waits
// at most the timeout once per TTL per op, and a sibling that is down costs one
// timeout per TTL rather than one per read. An unreachable sibling is a
// reported fact (`unknown`), never an error (ADR-0038).
package siblingfanout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// Defaults for [Reader]. The timeout is short because a person is waiting on
// the status read; the TTL is long enough that a page polling status does not
// re-dial every sibling on every poll.
const (
	DefaultTimeout = 2 * time.Second
	DefaultTTL     = 30 * time.Second
)

// maxAnswerBody bounds a sibling's answer. MaxFanOutApplications entries of a
// few hundred bytes each is well under it.
const maxAnswerBody = 1 << 20

// OutcomeUnknown is the outcome reported for a sibling that could not be
// asked, or did not answer the question.
const OutcomeUnknown = "unknown"

// OutcomePending is the outcome reported for a sibling that answered but has
// no outcome for the op: it has not evaluated it yet, usually because catalog
// sync has not carried it there.
const OutcomePending = "pending"

// Target is a sibling to ask: its pinned identity and where to dial it, kept
// together so a caller cannot pin one peer and dial another (ADR-0012).
type Target struct {
	Peer     mtls.Peer
	Endpoint string
}

// Targets lists the siblings to ask, read fresh so a removed member stops
// being asked.
type Targets func(ctx context.Context) ([]Target, error)

// Fetcher asks one sibling about one op. An interface so the reader can be
// tested without standing up mTLS; [Client] is the real implementation.
type Fetcher interface {
	Fetch(ctx context.Context, t Target, opHash string) (peerapi.FanOutAnswer, error)
}

// Outcome is one sibling's answer for one op, as the origin shows it.
type Outcome struct {
	PeerID string
	// Name is this node's name for the sibling, from membership.
	Name string
	// Site is the site the sibling says it is, "" when it did not answer.
	Site string
	// Reachable is whether the sibling answered at all.
	Reachable bool
	// Outcome is the sibling's outcome for the op, OutcomePending when it
	// answered with none, or OutcomeUnknown when it could not be asked.
	Outcome           string
	Detail            string
	AppliedAt         string
	State             string
	ReportedUnsourced bool
	// AskedAt is when this answer was fetched; a cached answer is older than
	// the read that returns it.
	AskedAt time.Time
}

// Reader asks every sibling about an op and caches the answers.
type Reader struct {
	fetch   Fetcher
	targets Targets
	timeout time.Duration
	ttl     time.Duration
	now     func() time.Time
	log     *slog.Logger

	mu    sync.Mutex
	cache map[cacheKey]Outcome
}

type cacheKey struct{ peerID, op string }

// Options configure a Reader. Zero Timeout, TTL and Now take the defaults.
type Options struct {
	Fetch   Fetcher
	Targets Targets
	Timeout time.Duration
	TTL     time.Duration
	Now     func() time.Time
	Logger  *slog.Logger
}

// NewReader builds a Reader.
func NewReader(o Options) *Reader {
	r := &Reader{
		fetch: o.Fetch, targets: o.Targets, timeout: o.Timeout, ttl: o.TTL, now: o.Now, log: o.Logger,
		cache: map[cacheKey]Outcome{},
	}
	if r.timeout <= 0 {
		r.timeout = DefaultTimeout
	}
	if r.ttl <= 0 {
		r.ttl = DefaultTTL
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	return r
}

// Outcomes is every current sibling's outcome for opHash, in the order Targets
// lists them. An unreachable sibling is an Outcome with Reachable false, not an
// error; the error is only for failing to list the siblings at all.
func (r *Reader) Outcomes(ctx context.Context, opHash string) ([]Outcome, error) {
	targets, err := r.targets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Outcome, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		if o, ok := r.cached(t.Peer.PeerID, opHash); ok {
			o.Name = t.Peer.Name
			out[i] = o
			continue
		}
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			out[i] = r.ask(ctx, t, opHash)
		}(i, t)
	}
	wg.Wait()
	return out, nil
}

func (r *Reader) cached(peerID, op string) (Outcome, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.cache[cacheKey{peerID, op}]
	if !ok || r.now().Sub(o.AskedAt) >= r.ttl {
		return Outcome{}, false
	}
	return o, true
}

// ask fetches one sibling's answer under the timeout and caches it, whatever
// it was. A cancelled caller is not cached: that says nothing about the
// sibling.
func (r *Reader) ask(ctx context.Context, t Target, opHash string) Outcome {
	o := Outcome{PeerID: t.Peer.PeerID, Name: t.Peer.Name, AskedAt: r.now()}
	actx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	ans, err := r.fetch.Fetch(actx, t, opHash)
	switch {
	case err != nil && ctx.Err() != nil:
		o.Outcome, o.Detail = OutcomeUnknown, "the status read ended before the sibling answered"
		return o
	case err != nil:
		o.Outcome = OutcomeUnknown
		var refused *RefusedError
		if errors.As(err, &refused) {
			// It answered, just not this question: an older build, or a node
			// that evaluates no siblings' ops.
			o.Reachable = true
			o.Detail = "the sibling did not answer the question: " + refused.Error()
		} else {
			o.Detail = "unreachable: " + err.Error()
		}
		r.log.Debug("a sibling's fan-out outcome is unknown", "peer_id", t.Peer.PeerID, "error", err)
	default:
		o.Reachable, o.Site, o.Outcome = true, ans.Site, OutcomePending
		for _, a := range ans.Applications {
			if a.OpHash != opHash {
				continue
			}
			o.Outcome, o.Detail, o.AppliedAt = a.Outcome, a.Detail, a.AppliedAt
			o.State, o.ReportedUnsourced = a.State, a.ReportedUnsourced
			break
		}
	}
	r.mu.Lock()
	r.cache[cacheKey{t.Peer.PeerID, opHash}] = o
	r.mu.Unlock()
	return o
}

// RefusedError is a sibling that answered with something other than 200.
type RefusedError struct {
	Status int
	Body   string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("answered %d: %s", e.Status, e.Body)
}

// Client is the mTLS-pinned [Fetcher]: it dials a sibling's peer surface with
// this node's certificate, pinned to the sibling's key (ADR-0012).
type Client struct {
	material *mtls.Material
	log      *slog.Logger
}

// NewClient builds a client that dials with the given certificate material.
func NewClient(material *mtls.Material, log *slog.Logger) *Client {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{material: material, log: log}
}

// Fetch asks the sibling what it did with opHash.
func (c *Client) Fetch(ctx context.Context, t Target, opHash string) (peerapi.FanOutAnswer, error) {
	hc, err := mtls.Client(mtls.Options{Material: c.material, Members: mtls.PinnedKey(t.Peer), Logger: c.log})
	if err != nil {
		return peerapi.FanOutAnswer{}, fmt.Errorf("siblingfanout: building a pinned client for %s: %w", t.Peer.PeerID, err)
	}
	defer func() {
		if tr, ok := hc.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}()
	origin, err := endpoint.Normalise(t.Endpoint)
	if err != nil {
		return peerapi.FanOutAnswer{}, fmt.Errorf("siblingfanout: peer %s has no usable endpoint: %w", t.Peer.PeerID, err)
	}
	u := strings.TrimSuffix(origin, "/") + peerapi.FanOutPath + "?op=" + url.QueryEscape(opHash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return peerapi.FanOutAnswer{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return peerapi.FanOutAnswer{}, fmt.Errorf("siblingfanout: asking %s: %w", t.Peer.PeerID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return peerapi.FanOutAnswer{}, &RefusedError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	var ans peerapi.FanOutAnswer
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswerBody)).Decode(&ans); err != nil {
		return peerapi.FanOutAnswer{}, &RefusedError{Status: resp.StatusCode, Body: "an answer that does not decode: " + err.Error()}
	}
	return ans, nil
}
