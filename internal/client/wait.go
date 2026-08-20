package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Waiting for a job to finish is the part of the CLI most likely to be subtly
// wrong, so the ordering is spelled out here rather than left to be inferred.
//
//  1. Subscribe to the event stream BEFORE reading the job's state. The other
//     order has a silent window: the job can finish between the state read and
//     the subscription, the event is emitted into nobody's connection, and the
//     wait never ends. This is the same ordering bug the server's own
//     /events handler documents at internal/api/resources/stream.go, one level
//     up — and Events() does not return until the subscription is confirmed,
//     so "before" here means before, not "issued before".
//  2. A job that is already terminal when the wait starts returns immediately.
//     Waiting for an event that was emitted an hour ago is a hang.
//  3. The event stream is a trigger, not the answer. Every trigger causes a
//     re-read of /jobs/{id}, which is the authority on state. That is what
//     makes the wait correct in the face of a dropped-events notice: the
//     stream says "your view is incomplete", and the only safe response is to
//     go and look rather than to assume nothing happened.
//  4. Nothing sleeps a fixed duration hoping something has become true. The
//     poll interval is a backstop for state transitions that emit no event,
//     and it is a re-read of the condition itself, never a bet on timing.
//
// The backstop is load-bearing today rather than belt-and-braces, for two
// reasons worth writing down because both are properties of the system rather
// than of this file:
//
//   - Nothing emits job.succeeded or job.failed yet. The constants exist
//     (internal/events) and the job queue's transitions do not emit them, so a
//     job reaching dead is a state change with no event behind it.
//   - The event log's live fan-out is in-process (events.Log.publish walks the
//     subscribers of that Log). Roles hold their own Log — even inside `heyarr
//     all` — so an event emitted by the worker or the scanner is durable
//     immediately and reaches an SSE subscriber only through a catch-up read
//     on the next connection, not live.
//
// So the stream accelerates the wait for anything the controller emits, and
// the poll is what actually notices a scan job finishing. PollInterval may be
// set to zero to wait purely on events, which is what the test that proves the
// subscribe-then-look ordering does; it is not what a command-line user should
// get.

// WaitEventTypes are the event types a job wait subscribes to. Filtering
// server-side keeps an unrelated flood of blob and asset events off this
// connection, which matters because a slow consumer is dropped rather than
// backpressured (ADR-0009).
var WaitEventTypes = []string{
	"job.succeeded",
	"job.failed",
	"job.enqueued",
	"system.scan.progress",
	"ingest.completed",
}

// DefaultPollInterval is the backstop re-read cadence.
const DefaultPollInterval = time.Second

// Progress reports what the wait has learned. It is called from the waiting
// goroutine, so an implementation that blocks stalls the wait.
type Progress struct {
	// Jobs is the current state of everything being waited on.
	Jobs []Job
	// Event is the event that prompted this report, if any.
	Event *Event
	// Gap is set when the server reported that it dropped events for this
	// connection. The wait re-reads job state rather than assuming the job is
	// fine, and the caller should say so out loud rather than hiding it.
	Gap *Gap
}

// WaitOptions configure a wait.
type WaitOptions struct {
	// PollInterval re-reads job state periodically as a backstop. Zero waits
	// purely on the event stream.
	PollInterval time.Duration
	// Progress, when set, is called on every state change and gap notice.
	Progress func(Progress)
	// AfterSubscribe is a seam for the one test that can prove the ordering.
	// It runs after the subscription exists and before the first state read —
	// precisely inside the window that subscribing first exists to close — so
	// a test can make the job finish there and assert that the wait still
	// ends. Production callers leave it nil.
	AfterSubscribe func()
}

// WaitResult is how the wait ended.
type WaitResult struct {
	// Jobs is the final state of every job waited on.
	Jobs []Job `json:"jobs"`
	// Succeeded and Dead count the outcomes. Both are reported rather than one
	// derived from the other, because "3 of 4 succeeded" is the sentence an
	// operator needs and "not all succeeded" is not.
	Succeeded int `json:"succeeded"`
	Dead      int `json:"dead"`
}

// Failed reports whether any job died. This is the exit code, and it is a
// method rather than a field so that no caller can construct a result that
// claims success while holding a dead job.
func (r WaitResult) Failed() bool { return r.Dead > 0 }

// WaitForJobs blocks until every job listed has reached a terminal state.
//
// It returns the final states. Deciding what to do about a dead job — the exit
// code, the message — belongs to the caller; this function's contract is that
// it does not return until it knows, and that what it returns is what the
// server said rather than what the stream implied.
func (c *Client) WaitForJobs(ctx context.Context, ids []string, opts WaitOptions) (WaitResult, error) {
	if len(ids) == 0 {
		return WaitResult{Jobs: []Job{}}, nil
	}

	// 1. Subscribe first. Everything below assumes no event can be emitted
	//    into the gap between here and the state read, and that assumption is
	//    only true because Events() has confirmed the subscription.
	stream, err := c.Events(ctx, 0, WaitEventTypes)
	if err != nil {
		return WaitResult{}, err
	}
	defer func() { _ = stream.Close() }()

	if opts.AfterSubscribe != nil {
		opts.AfterSubscribe()
	}

	// 2. Now look. A job that finished before the subscription existed is
	//    found here; a job that finishes after it is delivered as an event.
	//    There is no third case, which is the whole point of the order.
	result, done, err := c.readJobs(ctx, ids)
	if err != nil {
		return WaitResult{}, err
	}
	if opts.Progress != nil {
		opts.Progress(Progress{Jobs: result.Jobs})
	}
	if done {
		return result, nil
	}

	frames := make(chan StreamMessage, 64)
	streamErr := make(chan error, 1)
	go readFrames(stream, frames, streamErr)

	var tick <-chan time.Time
	if opts.PollInterval > 0 {
		ticker := time.NewTicker(opts.PollInterval)
		defer ticker.Stop()
		tick = ticker.C
	}

	for {
		var (
			event *Event
			gap   *Gap
		)

		select {
		case <-ctx.Done():
			return result, fmt.Errorf("gave up waiting for %s: %w", describeJobs(result.Jobs), ctx.Err())

		case msg := <-frames:
			// Coalesce whatever else is already queued: a scan emits progress
			// in bursts, and one re-read answers all of them.
			drain(frames)
			event, gap = msg.Event, msg.Gap

		case err := <-streamErr:
			// The server closes the stream after a gap notice, and a network
			// hiccup looks the same. Re-read state, then resume from the last
			// sequence seen — which is gapless, because the log is the record.
			var reopened *EventStream
			result, done, err = c.afterStreamEnded(ctx, err, ids)
			if done || err != nil {
				return result, err
			}
			reopened, err = c.Events(ctx, stream.LastSeq(), WaitEventTypes)
			if err != nil {
				return result, err
			}
			_ = stream.Close()
			stream = reopened
			go readFrames(stream, frames, streamErr)
			continue

		case <-tick:
		}

		// 3. Whatever prompted this, the answer comes from the job rows, not
		//    from the event. A gap notice in particular must never be read as
		//    "nothing happened".
		result, done, err = c.readJobs(ctx, ids)
		if err != nil {
			return result, err
		}
		if opts.Progress != nil {
			opts.Progress(Progress{Jobs: result.Jobs, Event: event, Gap: gap})
		}
		if done {
			return result, nil
		}
	}
}

// afterStreamEnded handles a stream that ended. It re-reads state first, so a
// job that finished while the connection was breaking is noticed rather than
// waited on.
func (c *Client) afterStreamEnded(ctx context.Context, cause error, ids []string) (WaitResult, bool, error) {
	result, done, err := c.readJobs(ctx, ids)
	if err != nil {
		return result, false, err
	}
	if done {
		return result, true, nil
	}
	if ctx.Err() != nil {
		return result, false, ctx.Err()
	}
	if cause != nil && !errors.Is(cause, io.EOF) && !errors.Is(cause, io.ErrUnexpectedEOF) {
		// Anything that is not a closed connection is not something reopening
		// will fix, and retrying it forever would turn a broken deployment into
		// a hang.
		return result, false, fmt.Errorf("the event stream failed: %w", cause)
	}
	return result, false, nil
}

// readJobs reads every job's current state and reports whether all are done.
func (c *Client) readJobs(ctx context.Context, ids []string) (WaitResult, bool, error) {
	out := WaitResult{Jobs: make([]Job, 0, len(ids))}
	done := true
	for _, id := range ids {
		var job Job
		if err := c.Get(ctx, "/jobs/"+id, nil, &job); err != nil {
			return WaitResult{}, false, err
		}
		out.Jobs = append(out.Jobs, job)
		switch job.State {
		case JobSucceeded:
			out.Succeeded++
		case JobDead:
			out.Dead++
		default:
			// pending, leased, and failed — failed is a spent attempt that the
			// queue will retry, not an outcome.
			done = false
		}
	}
	return out, done, nil
}

// readFrames pumps the stream into a channel so the wait loop can select on it
// alongside the ticker and the context.
func readFrames(stream *EventStream, out chan<- StreamMessage, errc chan<- error) {
	for {
		msg, err := stream.Recv()
		if err != nil {
			select {
			case errc <- err:
			default:
			}
			return
		}
		select {
		case out <- msg:
		default:
			// The consumer is behind. Dropping a trigger is safe — every
			// trigger causes the same re-read, and the ones still queued will
			// cause it — but dropping the *last* one would not be, so the
			// backstop poll exists.
		}
	}
}

func drain(frames chan StreamMessage) {
	for {
		select {
		case <-frames:
		default:
			return
		}
	}
}

// describeJobs renders what is still outstanding, for a timeout message that
// says which job rather than "a job".
func describeJobs(jobs []Job) string {
	if len(jobs) == 1 {
		return fmt.Sprintf("job %s (%s, %s)", jobs[0].ID, jobs[0].Type, jobs[0].State)
	}
	return fmt.Sprintf("%d jobs", len(jobs))
}

// ScanProgress is the payload of a system.scan.progress event, decoded for
// display. Fields absent from the payload stay zero.
type ScanProgress struct {
	RootID        string `json:"root_id"`
	State         string `json:"state"`
	FilesSeen     int64  `json:"files_seen"`
	FilesEnqueued int64  `json:"files_enqueued"`
	FilesSkipped  int64  `json:"files_skipped"`
	FilesMissing  int64  `json:"files_missing"`
	Error         string `json:"error,omitempty"`
}

// DecodeScanProgress reads a scan progress payload. It reports ok=false for any
// other event type rather than returning a zeroed struct that would print as
// "0 files seen".
func DecodeScanProgress(e *Event) (ScanProgress, bool) {
	if e == nil || e.Type != "system.scan.progress" || len(e.Payload) == 0 {
		return ScanProgress{}, false
	}
	var p ScanProgress
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return ScanProgress{}, false
	}
	return p, true
}
