package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
)

// RecordProbe stores a probe result against the blob it describes, and emits
// the event, in one transaction (§29, M2-04).
//
// It is an upsert, because a probe job WILL be re-run (invariant 9): a lease
// that expires mid-probe returns the job to the queue, and the second run must
// converge on the same row rather than fail on a primary key or accumulate a
// second answer.
//
// The event is emitted inside the transaction for the reason invariant 7
// exists: an event written outside it can describe a probe that rolled back,
// or — worse, because it is invisible — a probe that committed with no event
// at all. That is the failure the job queue needed a follow-up fix (#62) to
// close, and it is not being reopened here.
func (c *Catalog) RecordProbe(
	ctx context.Context, blobHash string, result probe.Result, stats probe.Stats, now time.Time,
) error {
	streams, err := json.Marshal(result.Streams)
	if err != nil {
		return fmt.Errorf("catalog: encoding the probe streams for %s: %w", blobHash, err)
	}

	var pending events.Event
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO blob_probes (blob_hash, container, format_long, duration_seconds,
				bitrate_bps, streams, bytes_read, materialised, probed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (blob_hash) DO UPDATE SET
				container = excluded.container,
				format_long = excluded.format_long,
				duration_seconds = excluded.duration_seconds,
				bitrate_bps = excluded.bitrate_bps,
				streams = excluded.streams,
				bytes_read = excluded.bytes_read,
				materialised = excluded.materialised,
				probed_at = excluded.probed_at`,
			blobHash, result.Container, result.FormatLong,
			nullableFloat(result.DurationSec), nullableInt(result.BitrateBPS),
			string(streams), stats.BytesRead, boolToInt(stats.Materialised),
			now.Format(timestampFormat)); err != nil {
			return fmt.Errorf("catalog: recording the probe for %s: %w", blobHash, err)
		}

		pending, err = c.events.EmitTx(ctx, tx, events.TypeBlobProbed, "blob", blobHash,
			map[string]any{
				"blob_hash": blobHash,
				"container": result.Container,
				"streams":   len(result.Streams),
				// The §29 evidence, on the event as well as in the row, so an
				// operator watching the stream can see whether remote probing
				// is actually working without querying anything.
				"bytes_read":   stats.BytesRead,
				"materialised": stats.Materialised,
			})
		return err
	})
	if err != nil {
		return err
	}
	c.events.Publish(pending)
	return nil
}

// nullableFloat and nullableInt keep "the container did not declare this"
// distinct from zero. A Matroska file legitimately has no overall bit rate,
// and a stored 0 would be a claim that it declared one.
func nullableFloat(v float64) any {
	if v <= 0 {
		return nil
	}
	return v
}

func nullableInt(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
