package worker

import (
	"encoding/json"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
)

// decodePayload decodes a job's payload into T. A payload that does not decode
// is a property of the job's INPUT, and a job's payload never changes once it
// is enqueued — so it will fail identically on every attempt. The error wraps
// jobs.ErrPermanent and the job goes straight to dead instead of spending its
// remaining attempts rediscovering the same answer (see jobs.ErrPermanent).
// An operator who has fixed whatever enqueued it can still `jobs retry` it.
//
// An empty payload is decoded like any other, and so is refused: a handler
// whose payload is genuinely optional wants decodeOptionalPayload.
func decodePayload[T any](job jobs.Job) (T, error) {
	var payload T
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return payload, fmt.Errorf("%w: worker: %s payload is not decodable: %w",
			jobs.ErrPermanent, payloadJobType(job), err)
	}
	return payload, nil
}

// decodeOptionalPayload is decodePayload for a handler that treats an absent
// payload as "all of them": an empty payload is T's zero value, and anything
// present must still decode.
func decodeOptionalPayload[T any](job jobs.Job) (T, error) {
	if len(job.Payload) == 0 {
		var zero T
		return zero, nil
	}
	return decodePayload[T](job)
}

// payloadJobType names the job in a decode error. A job read from the queue
// always has a type; one built by hand in a test may not.
func payloadJobType(job jobs.Job) string {
	if job.Type == "" {
		return "job"
	}
	return job.Type
}
