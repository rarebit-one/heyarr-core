package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

// Path is where a controller accepts an inventory report, under the peer
// surface's prefix. It is a constant so the two ends cannot disagree about it.
const Path = "/peer/v1/inventory"

// maxErrorBody bounds how much of a controller's refusal is read back into an
// error message. A problem document is a few hundred bytes; anything larger is
// not something worth putting in a log line.
const maxErrorBody = 8 << 10

// Reporter is the peer half of the exchange: it observes this node's store and
// tells the controller what it found (ADR-0029 — the peer executes, and
// anything the control plane must know travels to the controller over the API
// and is written there, by the single writer).
//
// # Why it holds the previous snapshot
//
// An incremental report is a diff, and a diff needs something to diff against.
// The peer keeps its last observation in memory rather than in a local
// database, because it is a CACHE and not control state (ADR-0029): losing it
// costs one full report, which is the correct and safe failure. A peer that
// persisted it would have to be right about it across restarts, and being
// wrong would mean silently never reporting a loss.
//
// # Why it still sends a full report periodically
//
// Incremental reports are correct only if every previous one arrived and was
// applied. A dropped cycle, a controller restored from a backup, or a row
// changed by anything else leaves the controller's belief and this peer's disk
// diverged with nothing to notice it — because a diff of two identical
// snapshots is empty, and an empty incremental report asserts nothing. The
// periodic full report is the drift corrector: it is the only shape that can
// say "and nothing else".
type Reporter struct {
	client     *http.Client
	controller string
	peerID     string
	collect    func(context.Context) (Snapshot, error)
	fullEvery  int
	log        *slog.Logger

	mu       sync.Mutex
	cycles   int
	previous Snapshot
	hasPrev  bool
}

// DefaultFullEvery is how many cycles pass between full reports.
//
// On a cycle a peer runs hourly this is a full reconciliation daily: often
// enough that drift is measured in hours rather than in "until somebody looked",
// and rarely enough that a large library is not shipped in its entirety every
// time — which is the whole reason incremental reporting exists (§20: "Peers
// should exchange only missing content where possible").
const DefaultFullEvery = 24

// ReporterOptions configure a Reporter.
type ReporterOptions struct {
	// Client is a PINNED peer client (internal/peer/mtls.Client). Required.
	// Nothing here builds one: a caller that passed an ordinary http.Client
	// would report this node's entire inventory to whoever answered at the
	// address, and the traffic would look identical until the day it mattered.
	Client *http.Client
	// ControllerURL is the base URL of the controller's peer surface, e.g.
	// https://controller.example:9443. Required.
	ControllerURL string
	// PeerID is this node's membership id. It is sent as a DECLARATION and the
	// controller compares it against the certificate (ADR-0033); it is never
	// what makes the report land under this peer.
	PeerID string
	// Collect observes this node's store. Required.
	Collect func(context.Context) (Snapshot, error)
	// FullEvery is how many cycles between full reports. Zero means
	// DefaultFullEvery; one means every report is full.
	FullEvery int
	Logger    *slog.Logger
}

// NewReporter builds a Reporter.
func NewReporter(opts ReporterOptions) (*Reporter, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("inventory: a pinned peer client is required — " +
			"an inventory report names every blob this node holds, and an unpinned " +
			"transport would offer that list to whoever answered (ADR-0012)")
	case opts.ControllerURL == "":
		return nil, errors.New("inventory: the controller's peer surface URL is required")
	case opts.PeerID == "":
		return nil, errors.New("inventory: this node's peer id is required")
	case opts.Collect == nil:
		return nil, errors.New("inventory: a collector is required — an inventory is what is on disk")
	}
	every := opts.FullEvery
	if every <= 0 {
		every = DefaultFullEvery
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Reporter{
		client:     opts.Client,
		controller: strings.TrimRight(opts.ControllerURL, "/"),
		peerID:     opts.PeerID,
		collect:    opts.Collect,
		fullEvery:  every,
		log:        log.With("component", "inventory"),
	}, nil
}

// Cycle observes the store once and reports it.
//
// The first cycle is always full — there is nothing to diff against, and an
// incremental report from a peer the controller has never heard from would
// leave every blob it holds unmentioned and therefore unconfirmed. Every
// FullEvery-th cycle after that is full as well, for the drift reason above.
//
// The previous snapshot is replaced only on success. A report that did not
// reach the controller must not be treated as applied: the next cycle's diff
// would be computed against a state the controller never reached, and the
// changes in between would never be reported again.
func (r *Reporter) Cycle(ctx context.Context) (Outcome, error) {
	current, err := r.collect(ctx)
	if err != nil {
		return Outcome{}, err
	}

	r.mu.Lock()
	previous, hasPrev, cycles := r.previous, r.hasPrev, r.cycles
	r.mu.Unlock()

	report := current.Full(r.peerID)
	if hasPrev && cycles%r.fullEvery != 0 {
		report = current.Since(previous, r.peerID)
	}

	outcome, err := r.send(ctx, report)
	if err != nil {
		return Outcome{}, err
	}

	r.mu.Lock()
	r.previous, r.hasPrev, r.cycles = current, true, cycles+1
	r.mu.Unlock()

	r.log.Info("reported inventory to the controller",
		"mode", outcome.Mode, "entries", outcome.Entries, "added", outcome.Added,
		"changed", outcome.Changed, "removed", outcome.Removed, "unknown", outcome.Unknown)
	return outcome, nil
}

// ReportFull observes the store and sends a full report, whatever the cycle
// count says. It is what an operator runs when they want the controller's
// belief repaired now rather than at the next scheduled full cycle.
func (r *Reporter) ReportFull(ctx context.Context) (Outcome, error) {
	current, err := r.collect(ctx)
	if err != nil {
		return Outcome{}, err
	}
	outcome, err := r.send(ctx, current.Full(r.peerID))
	if err != nil {
		return Outcome{}, err
	}
	r.mu.Lock()
	r.previous, r.hasPrev, r.cycles = current, true, r.cycles+1
	r.mu.Unlock()
	return outcome, nil
}

// send puts one report on the wire.
func (r *Reporter) send(ctx context.Context, report Report) (Outcome, error) {
	if err := report.Validate(); err != nil {
		// Validated before it leaves, so a malformed report fails where it was
		// built rather than as a 400 from another machine.
		return Outcome{}, err
	}
	body, err := json.Marshal(report)
	if err != nil {
		return Outcome{}, fmt.Errorf("inventory: encoding the report: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.controller+Path, bytes.NewReader(body))
	if err != nil {
		return Outcome{}, fmt.Errorf("inventory: building the report request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return Outcome{}, fmt.Errorf("inventory: reporting to %s: %w", r.controller, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return Outcome{}, fmt.Errorf("inventory: the controller refused this report with %d: %s",
			resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	var outcome Outcome
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&outcome); err != nil {
		return Outcome{}, fmt.Errorf("inventory: the controller answered something that is not an outcome: %w", err)
	}
	if outcome.PeerID != r.peerID {
		// The controller records against the peer the CERTIFICATE proved. If
		// that is not this peer, this node's identity and its configuration
		// disagree, and its inventory just landed somewhere it did not mean —
		// which is worth failing on rather than logging.
		return Outcome{}, fmt.Errorf("inventory: this node reported as %s and the controller "+
			"recorded the report against %s — the certificate and this node's configuration "+
			"name different peers (ADR-0033)", r.peerID, outcome.PeerID)
	}
	return outcome, nil
}
