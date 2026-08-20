package worker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/scanner"
)

// ScanHandler walks one library root for one scan_library job.
//
// Like IngestHandler, it is a decode and a delegation: a handler that contains
// the logic is a handler that cannot be tested without a queue.
//
// The job's context is tied to the lease, so a worker that loses its lease or
// is asked to drain stops mid-walk. That is safe rather than wasteful because
// the fingerprint cache is written as the scan goes: the next run picks up
// where this one stopped instead of re-reading what already landed (M1-12).
func ScanHandler(s *scanner.Scanner) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		var payload scanner.Payload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("worker: scan_library payload is not decodable: %w", err)
		}
		_, err := s.Scan(ctx, payload)
		return err
	}
}
