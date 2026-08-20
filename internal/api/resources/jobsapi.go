package resources

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
)

// jobFromQueue converts the queue's job to the wire shape. The two are
// deliberately separate types: the queue's is a persistence concern and this
// one is a contract.
func jobFromQueue(j jobs.Job) Job {
	out := Job{
		ID:                 j.ID,
		Type:               j.Type,
		Payload:            j.Payload,
		State:              string(j.State),
		Priority:           j.Priority,
		DedupeKey:          emptyToNil(j.DedupeKey),
		RequiredCapability: j.RequiredCapability,
		RunAfter:           j.RunAfter.UTC(),
		Attempts:           j.Attempts,
		MaxAttempts:        j.MaxAttempts,
		LeaseOwner:         emptyToNil(j.LeaseOwner),
		LastError:          emptyToNil(j.LastError),
		CreatedAt:          j.CreatedAt.UTC(),
		UpdatedAt:          j.UpdatedAt.UTC(),
	}
	if len(out.Payload) == 0 {
		out.Payload = json.RawMessage("{}")
	}
	if !j.LeaseExpiresAt.IsZero() {
		t := j.LeaseExpiresAt.UTC()
		out.LeaseExpiresAt = &t
	}
	if !j.FinishedAt.IsZero() {
		t := j.FinishedAt.UTC()
		out.FinishedAt = &t
	}
	return out
}

const jobColumns = `id, type, payload, state, priority, dedupe_key, required_capability,
	run_after, attempts, max_attempts, lease_owner, lease_expires_at, last_error,
	created_at, updated_at, finished_at`

func scanJobRow(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var payload string
	var dedupe, leaseOwner, leaseExpires, lastError, finished sql.NullString
	var runAfter, created, updated string
	if err := row.Scan(&j.ID, &j.Type, &payload, &j.State, &j.Priority, &dedupe,
		&j.RequiredCapability, &runAfter, &j.Attempts, &j.MaxAttempts,
		&leaseOwner, &leaseExpires, &lastError, &created, &updated, &finished); err != nil {
		return Job{}, err
	}
	j.Payload = json.RawMessage(payload)
	j.DedupeKey = nullString(dedupe)
	j.RunAfter = parseTime(runAfter)
	j.LeaseOwner = nullString(leaseOwner)
	j.LeaseExpiresAt = parseNullTime(leaseExpires)
	j.LastError = nullString(lastError)
	j.CreatedAt = parseTime(created)
	j.UpdatedAt = parseTime(updated)
	j.FinishedAt = parseNullTime(finished)
	return j, nil
}

// listJobs pages by id, which is a UUIDv7 and therefore already in enqueue
// order.
func (a *API) listJobs(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, "jobs", 1)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	state, err := oneOf(r, "state", "pending", "leased", "succeeded", "failed", "dead")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	where := []string{"1 = 1"}
	args := []any{}
	if state != "" {
		where = append(where, "state = ?")
		args = append(args, state)
	}
	if t := r.URL.Query().Get("type"); t != "" {
		where = append(where, "type = ?")
		args = append(args, t)
	}
	if q.cursor != nil {
		where = append(where, "id > ?")
		args = append(args, q.cursor[0])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // the query is assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + jobColumns + ` FROM jobs WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY id ASC LIMIT ?`

	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "job", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var out []Job
	for rows.Next() {
		j, err := scanJobRow(rows)
		if err != nil {
			a.fail(w, r, "job", err)
			return
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "job", err)
		return
	}
	a.write(w, r, http.StatusOK, newPage(out, q.limit,
		func(x Job) []string { return []string{x.ID} }, "jobs"))
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	row := a.reader.QueryRowContext(r.Context(),
		`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, chi.URLParam(r, "id"))
	j, err := scanJobRow(row)
	if err != nil {
		a.fail(w, r, "job", err)
		return
	}
	a.write(w, r, http.StatusOK, j)
}

// retryJob puts a finished job back on the queue. It is an operator action:
// something was wrong and has been fixed.
func (a *API) retryJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// Look first, so that "no such job" is a 404 and "that job is still
	// running" is a 409. The queue conflates the two into one error because a
	// worker's required action is the same for both; an operator's is not.
	existing, err := a.jobs.Get(r.Context(), id)
	if err != nil {
		a.fail(w, r, "job", err)
		return
	}
	if err := a.jobs.Retry(r.Context(), id); err != nil {
		if errors.Is(err, jobs.ErrLeaseLost) {
			httpapi.Fail(w, r, problem.Conflict(
				"this job is "+string(existing.State)+"; only a succeeded, failed or dead job can be retried"))
			return
		}
		a.fail(w, r, "job", err)
		return
	}

	updated, err := a.jobs.Get(r.Context(), id)
	if err != nil {
		a.fail(w, r, "job", err)
		return
	}
	// The retry is a state transition, so it emits (invariant 7). It is emitted
	// after the queue's own write rather than inside it because the queue owns
	// its transaction and reaching into it from here would couple the two.
	if _, err := a.events.Emit(r.Context(), events.TypeJobEnqueued, "job", id,
		map[string]any{
			"job_id": id, "type": updated.Type, "reason": "retried by an operator",
			"previous_state": string(existing.State),
		}); err != nil {
		a.log.Error("a job was retried but its event was not recorded",
			"request_id", httpapi.RequestIDFrom(r.Context()), "job_id", id, "error", err)
	}
	a.write(w, r, http.StatusOK, jobFromQueue(updated))
}

// RemuxRequest is the POST /playback/remux body.
type RemuxRequest struct {
	AssetID  string `json:"asset_id"`
	DeviceID string `json:"device_id"`
}

// enqueueRemux queues a remux for an asset a device cannot take as it is
// (§10, §75, M2-10).
//
// It is a separate call from POST /playback rather than something that
// endpoint does automatically, and that is deliberate for Milestone 2: a
// remux is minutes of work and disk, and starting one because a client asked
// what it could play would let a browsing client fill a disk. Deciding when to
// spend that is a policy question, and policy is §55's, not this endpoint's.
//
// It returns the job. The client polls /jobs/{id} or follows job.* on the
// event stream, and re-plans when it succeeds.
func (a *API) enqueueRemux(w http.ResponseWriter, r *http.Request) {
	var body RemuxRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	for _, f := range []struct{ name, value string }{
		{"asset_id", body.AssetID}, {"device_id", body.DeviceID},
	} {
		if err := required(f.name, f.value); err != nil {
			httpapi.Fail(w, r, problem.BadRequest(err.Error()))
			return
		}
	}

	device, err := a.deviceProfile(r, body.DeviceID)
	if err != nil {
		a.fail(w, r, "device", err)
		return
	}
	media, blobHash, err := a.mediaProfile(r, body.AssetID)
	if err != nil {
		a.fail(w, r, "asset", err)
		return
	}
	replicas, err := a.replicasFor(r, blobHash)
	if err != nil {
		a.fail(w, r, "replica", err)
		return
	}

	plan := playback.Choose(media, device, replicas)
	// Only a REMUX plan is actionable in Milestone 2. Queueing work for a
	// DIRECT plan would be spending minutes of disk to produce a file nobody
	// needs, and queueing it for a TRANSCODE plan would produce a remux that
	// still does not play — a failure the client would discover only after
	// waiting for it.
	if plan.Decision != playback.DecisionRemux {
		httpapi.Fail(w, r, problem.Conflict(
			"this asset plans "+string(plan.Decision)+" on this device, and Milestone 2 remuxes only; "+
				"POST /api/v1/playback/plan for the full rationale"))
		return
	}

	target := ffmpeg.TargetFor(device.Containers)
	job, err := a.jobs.Enqueue(r.Context(), jobs.EnqueueOptions{
		Type: ffmpeg.JobType,
		Payload: ffmpeg.Payload{
			BlobHash: blobHash, AssetID: body.AssetID, Container: target,
		},
		DedupeKey:          ffmpeg.DedupeKey(blobHash, target),
		RequiredCapability: ffmpeg.Capability,
	})
	if err != nil {
		a.fail(w, r, "job", err)
		return
	}
	w.Header().Set("Location", httpapi.APIPrefix+"/jobs/"+job.ID)
	a.write(w, r, http.StatusAccepted, map[string]any{
		"job_id": job.ID, "container": string(target), "asset_id": body.AssetID,
	})
}
