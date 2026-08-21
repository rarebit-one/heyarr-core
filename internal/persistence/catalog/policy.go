package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// SeedQualityProfiles makes sure a fresh Heyarr has profiles to point a
// DesiredItem at (§62, M3-01).
//
// # Converge on the name, and never overwrite an edit
//
// Seeding runs at every controller start, not only the first, and it converges
// on the profile NAME. That means a restart cannot produce a second
// `living-room` and leave DesiredItems pointing at whichever one they found.
//
// What it deliberately does NOT do is update a profile that already exists.
// An operator who tunes `living-room` keeps their tuning; a seeder that
// rewrote it every morning would silently revert their work, and they would
// discover it by wondering why the wrong releases keep arriving. The `seeded`
// column exists so that a future migration CAN distinguish an untouched
// default from an authored profile if it ever needs to — but the safe default
// is to leave rows alone once they exist.
//
// # Why here and not in the migration
//
// A migration cannot mint a UUIDv7 (ADR-0017), and a migration that inserted
// fixed identifiers would give every Heyarr in the world the same profile ids.
// This is the same shape as the self peer: create on first use, tolerate
// losing the race to another role, and emit.
func (c *Catalog) SeedQualityProfiles(ctx context.Context, profiles []policy.Profile) (int, error) {
	created := 0
	for i := range profiles {
		p := profiles[i]
		// A default that does not satisfy its own validator would fail on
		// someone else's machine rather than in our tests. Refuse loudly.
		if err := p.Validate(); err != nil {
			return created, fmt.Errorf("catalog: the seeded quality profile %q is not valid: %w", p.Name, err)
		}
		made, err := c.seedOneProfile(ctx, p)
		if err != nil {
			return created, err
		}
		if made {
			created++
		}
	}
	if created > 0 {
		c.log.Info("seeded quality profiles", "created", created, "total", len(profiles))
	}
	return created, nil
}

func (c *Catalog) seedOneProfile(ctx context.Context, p policy.Profile) (bool, error) {
	accept, prefer, terminal, err := encodeSections(p)
	if err != nil {
		return false, err
	}
	id := uuid.Must(uuid.NewV7()).String()
	now := c.clock.Now().Format(timestampFormat)

	var (
		ev      events.Event
		created bool
	)
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		res, execErr := tx.ExecContext(ctx, `
			INSERT INTO quality_profiles
				(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
			ON CONFLICT (name) DO NOTHING`,
			id, p.Name, p.Description, accept, prefer, terminal, now, now)
		if execErr != nil {
			return fmt.Errorf("catalog: seeding the quality profile %q: %w", p.Name, execErr)
		}
		n, execErr := res.RowsAffected()
		if execErr != nil {
			return fmt.Errorf("catalog: seeding the quality profile %q: %w", p.Name, execErr)
		}
		if n == 0 {
			// It already exists. Not an error, and not an event: nothing
			// transitioned. Emitting here would put three events in the log on
			// every single start, which is how an event stream becomes a
			// heartbeat nobody follows.
			return nil
		}
		created = true
		// Invariant 7, and inside the transaction that wrote the row.
		ev, execErr = c.events.EmitTx(ctx, tx, events.TypeQualityProfileCreated,
			"quality_profile", id,
			map[string]any{"quality_profile_id": id, "name": p.Name, "seeded": true})
		return execErr
	})
	if err != nil {
		// Losing the race to another role is ordinary — the row exists, which
		// is all this function wanted.
		if exists, checkErr := c.qualityProfileExists(ctx, p.Name); checkErr == nil && exists {
			return false, nil
		}
		return false, err
	}
	if created {
		c.events.Publish(ev)
	}
	return created, nil
}

func (c *Catalog) qualityProfileExists(ctx context.Context, name string) (bool, error) {
	var one int
	err := c.db.Reader().QueryRowContext(ctx,
		`SELECT 1 FROM quality_profiles WHERE name = ?`, name).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// encodeSections renders the three rule sections for storage.
//
// nil and an empty slice both become `[]` rather than `null`, because an
// absent `terminal` and an empty one are the same statement — "there is no
// condition under which this profile is finished" — and every read path would
// otherwise have to handle both spellings of it.
func encodeSections(p policy.Profile) (accept, prefer, terminal string, err error) {
	enc := func(rules []policy.Rule) (string, error) {
		if rules == nil {
			rules = []policy.Rule{}
		}
		raw, marshalErr := json.Marshal(rules)
		if marshalErr != nil {
			return "", fmt.Errorf("catalog: encoding quality profile rules: %w", marshalErr)
		}
		return string(raw), nil
	}
	if accept, err = enc(p.Accept); err != nil {
		return "", "", "", err
	}
	if prefer, err = enc(p.Prefer); err != nil {
		return "", "", "", err
	}
	if terminal, err = enc(p.Terminal); err != nil {
		return "", "", "", err
	}
	return accept, prefer, terminal, nil
}
