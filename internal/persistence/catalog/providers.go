package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Provider health, persisted (§59, M3-07).
//
// # Why this is stored at all
//
// The registry holds health in memory, which is enough for the process that
// did the checking. It is not enough for the process ANSWERING — under
// ADR-0002 the worker that runs the health job and the controller that serves
// GET /api/v1/providers may be different machines, and a health answer that
// never left the checker's memory would make the status endpoint report
// "never checked" forever on any split deployment.
//
// It also has to survive a restart. Otherwise every restart reports every
// provider as never-checked, and an operator watching a flapping indexer loses
// the history at exactly the moment they need it.
//
// # No credential is written here, ever
//
// Not encrypted, not hashed, not redacted-but-present. The plaintext lives in
// the operator's configuration file; a second copy in a database that gets
// backed up to peers (§50) would be a new way to leak it in exchange for
// nothing. providers.Secret keeps it out of logs and responses, and this file
// keeps it out of the backup stream — note that nothing below reads APIKey.

// RecordProviderHealth writes what a health pass observed.
//
// One transaction for the whole pass, so a reader never sees half a sweep: an
// operator refreshing the status page during a check should see the previous
// answer or the new one, not a mixture that suggests two providers changed
// state together when they did not.
func (c *Catalog) RecordProviderHealth(ctx context.Context, statuses []providers.Status) error {
	if len(statuses) == 0 {
		return nil
	}
	now := c.clock.Now().Format(timestampFormat)

	return c.db.InTx(ctx, func(tx *sql.Tx) error {
		for _, s := range statuses {
			caps := make([]string, 0, len(s.Capabilities))
			for _, cap := range s.Capabilities {
				caps = append(caps, string(cap))
			}
			encoded, err := json.Marshal(caps)
			if err != nil {
				return fmt.Errorf("catalog: encoding provider capabilities: %w", err)
			}

			healthy := 0
			if s.Health.Healthy {
				healthy = 1
			}
			var checkedAt any
			if s.Health.Checked() {
				checkedAt = s.Health.CheckedAt.Format(timestampFormat)
			}

			// An upsert on the name, which is the operator's word for this
			// provider and its identity everywhere else. created_at survives so
			// "since when have we known about this?" stays answerable.
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO provider_health
					(name, capabilities, healthy, detail, version, checked_at, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (name) DO UPDATE SET
					capabilities = excluded.capabilities,
					healthy      = excluded.healthy,
					detail       = excluded.detail,
					version      = excluded.version,
					checked_at   = excluded.checked_at,
					updated_at   = excluded.updated_at`,
				s.Name, string(encoded), healthy, s.Health.Detail, s.Health.Version,
				checkedAt, now, now); err != nil {
				return fmt.Errorf("catalog: recording health for provider %q: %w", s.Name, err)
			}
		}
		return nil
	})
}

// ProviderHealth reads what the last pass observed, by provider name.
//
// Returns only what has actually been recorded. A provider that is configured
// but has never been checked has no row, and the caller renders it as unknown —
// which is different from unhealthy, and the difference is the same one §56's
// satisfaction axes make.
func (c *Catalog) ProviderHealth(ctx context.Context) (map[string]providers.Health, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT name, healthy, detail, version, checked_at
		FROM provider_health`)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading provider health: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]providers.Health{}
	for rows.Next() {
		var (
			name, detail, version string
			healthy               int
			checkedAt             sql.NullString
		)
		if err := rows.Scan(&name, &healthy, &detail, &version, &checkedAt); err != nil {
			return nil, err
		}
		h := providers.Health{
			Healthy: healthy == 1,
			Detail:  detail,
			Version: version,
		}
		if checkedAt.Valid {
			// A stored timestamp that will not parse is corruption, not a
			// reason to fail the read: the health answer is still useful, and
			// a zero CheckedAt renders as "never checked", which is honest
			// about what we can tell.
			if t, err := time.Parse(timestampFormat, checkedAt.String); err == nil {
				h.CheckedAt = t
			}
		}
		out[name] = h
	}
	return out, rows.Err()
}
