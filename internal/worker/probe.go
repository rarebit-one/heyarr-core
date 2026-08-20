package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
)

// ProbeRecorder stores a probe result. It is an interface so the handler can be
// tested without a database in the way — the interesting behaviour here is the
// token, the URL and the failure classification, not SQL.
type ProbeRecorder interface {
	RecordProbe(ctx context.Context, blobHash string, result probe.Result, stats probe.Stats, now time.Time) error
}

// ProbeMinter issues the short-lived credential a probe uses.
type ProbeMinter interface {
	Create(ctx context.Context, name string, scopes []auth.Scope, expiresAt *time.Time) (auth.CreatedToken, error)
}

// probeTokenTTL is how long a probe credential lives.
//
// Long enough to cover a fallback that materialises a large blob over a slow
// link, short enough that a leaked one is worthless by the time anyone finds
// it. It is minted per probe rather than per worker: a long-lived credential
// sitting in a worker's memory for the life of the process is the thing this
// avoids, and minting is one insert.
const probeTokenTTL = 30 * time.Minute

// ProbeHandlerOptions configure the probe_blob handler.
type ProbeHandlerOptions struct {
	Prober   *probe.Prober
	Recorder ProbeRecorder
	Tokens   ProbeMinter
	// BaseURL is this node's API base, already resolved from the peer endpoint.
	BaseURL string
	Now     func() time.Time
	Logger  *slog.Logger
}

// ProbeHandler runs one probe_blob job.
//
// # What failure means here
//
// A probe that cannot describe the target is a FAILED job, not a silent
// success: the planner would otherwise be handed nothing and have to guess,
// and a guess about a file nobody could read is the worst kind. But it is
// failed with a typed error, so a queue retry is what happens to a network
// blip while a genuinely unreadable file exhausts its attempts and stops —
// which is the difference between a transient and a permanent condition, and
// the queue already knows how to tell them apart.
func ProbeHandler(opts ProbeHandlerOptions) HandlerFunc {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return func(ctx context.Context, job jobs.Job) error {
		var payload probe.Payload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			// A payload that cannot be decoded will never decode. Retrying it
			// is five guaranteed failures and a delay before the same answer.
			return fmt.Errorf("probe: undecodable payload: %w", err)
		}
		if payload.BlobHash == "" {
			return errors.New("probe: the payload names no blob")
		}

		// Scoped to read and expiring, minted for this probe alone. It reaches
		// the loopback proxy and never ffprobe's argv, where it would be
		// world-readable in the process table (see internal/media/probe).
		expires := now().Add(probeTokenTTL)
		token, err := opts.Tokens.Create(ctx,
			"probe "+payload.BlobHash[:min(len(payload.BlobHash), 20)],
			[]auth.Scope{auth.ScopeRead}, &expires)
		if err != nil {
			return fmt.Errorf("probe: minting a credential: %w", err)
		}

		result, stats, err := opts.Prober.Probe(ctx, probe.Target{
			URL:   probe.BlobURL(opts.BaseURL, payload.BlobHash),
			Token: token.Secret,
			Size:  payload.Size,
		})
		if err != nil {
			log.Warn("a blob could not be probed",
				"blob", payload.BlobHash, "bytes_read", stats.BytesRead, "error", err)
			return err
		}

		if err := opts.Recorder.RecordProbe(ctx, payload.BlobHash, result, stats, now()); err != nil {
			return err
		}

		log.Info("probed",
			"blob", payload.BlobHash,
			"container", result.Container,
			"streams", len(result.Streams),
			"bytes_read", stats.BytesRead,
			"materialised", stats.Materialised,
			"elapsed", stats.Elapsed)
		return nil
	}
}
