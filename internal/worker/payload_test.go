package worker

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
)

type testPayload struct {
	Name string `json:"name"`
}

func TestDecodePayload(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		optional  bool
		want      string
		permanent bool
	}{
		{name: "decodes", payload: `{"name":"a"}`, want: "a"},
		{name: "wrong type is permanent", payload: `{"name":12}`, permanent: true},
		{name: "truncated is permanent", payload: `{"name":`, permanent: true},
		{name: "empty is refused when required", payload: ``, permanent: true},
		{name: "empty is the zero value when optional", payload: ``, optional: true},
		{name: "optional still refuses garbage", payload: `nope`, optional: true, permanent: true},
		{name: "optional decodes", payload: `{"name":"b"}`, optional: true, want: "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := jobs.Job{Type: "scan_library", Payload: json.RawMessage(tc.payload)}
			decode := decodePayload[testPayload]
			if tc.optional {
				decode = decodeOptionalPayload[testPayload]
			}
			got, err := decode(job)
			if tc.permanent {
				if !errors.Is(err, jobs.ErrPermanent) {
					t.Fatalf("err = %v, want it to wrap jobs.ErrPermanent", err)
				}
				if !strings.Contains(err.Error(), "worker: scan_library payload is not decodable") {
					t.Errorf("err = %v, which does not name the job type", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Name != tc.want {
				t.Errorf("Name = %q, want %q", got.Name, tc.want)
			}
		})
	}
}

// End to end through the queue: a job whose payload does not decode is dead
// after ONE attempt, not after walking its whole retry budget to the same
// answer. The payload is immutable once enqueued, so the answer cannot change.
func TestAnUndecodablePayloadGoesStraightToDead(t *testing.T) {
	h := newHarness(t)
	queue, err := jobs.New(jobs.Options{Writer: h.db.Writer(), Reader: h.db.Reader(), Events: h.events})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(t.Context(), jobs.EnqueueOptions{
		Type:        ingest.JobType,
		Payload:     json.RawMessage(`{"root_id": 12}`),
		MaxAttempts: 5,
	}); err != nil {
		t.Fatal(err)
	}
	job, err := queue.Claim(t.Context(), jobs.ClaimOptions{Owner: "w"})
	if err != nil {
		t.Fatal(err)
	}

	cause := IngestHandler(h.pipeline, nil)(t.Context(), job)
	if !errors.Is(cause, jobs.ErrPermanent) {
		t.Fatalf("handler returned %v, want it to wrap jobs.ErrPermanent", cause)
	}
	if err := queue.Fail(t.Context(), job.ID, "w", cause); err != nil {
		t.Fatal(err)
	}

	got, err := queue.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != jobs.Dead {
		t.Fatalf("state = %s after one undecodable attempt of a 5-attempt job, want dead", got.State)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
	if !strings.Contains(got.LastError, "not decodable") {
		t.Errorf("last_error = %q, which does not say what happened", got.LastError)
	}
}
