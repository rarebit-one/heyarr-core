package worker

import (
	"log/slog"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Both workers are long-running: they wait ServiceWriteWait for another
// process's write lock, not the minute a one-shot command waits (#910).
func TestWorkerOpensWithTheServiceWriteWait(t *testing.T) {
	opts := databaseOptions("/srv/heyarr/heyarr.db", slog.New(slog.DiscardHandler))
	if opts.WriteWait != sqlite.ServiceWriteWait {
		t.Errorf("WriteWait = %s, want %s", opts.WriteWait, sqlite.ServiceWriteWait)
	}
}
