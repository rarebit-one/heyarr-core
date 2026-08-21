package controller

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Seeding runs at EVERY start, not only the first. These tests are about the
// hundredth start, because the first one is the easy case.

func seedHarness(t *testing.T) (*sqlite.DB, config.Config) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Database.Path = filepath.Join(dir, "heyarr.db")
	cfg.Peer.Name = "test-peer"
	cfg.Peer.Site = "test-site"

	db, err := sqlite.Open(ctx, sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db, cfg
}

func profileCount(t *testing.T, db *sqlite.DB) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM quality_profiles`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func eventCount(t *testing.T, db *sqlite.DB) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A fresh Heyarr has profiles to point a DesiredItem at, and a restart does not
// produce a second copy of each.
func TestSeedingIsIdempotentAcrossRestarts(t *testing.T) {
	db, cfg := seedHarness(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)

	if err := seedQualityProfiles(ctx, db, cfg, log); err != nil {
		t.Fatal(err)
	}
	first := profileCount(t, db)
	if first != len(policy.Defaults()) {
		t.Fatalf("a fresh database should hold %d seeded profiles, got %d",
			len(policy.Defaults()), first)
	}
	eventsAfterFirst := eventCount(t, db)
	if eventsAfterFirst < first {
		t.Fatalf("every seeded profile is a state transition and must emit (invariant 7): "+
			"%d profiles produced %d events", first, eventsAfterFirst)
	}

	// The hundredth start. Nothing new, and — just as important — NOTHING
	// EMITTED. Three events per start is how an event log becomes a heartbeat
	// nobody follows.
	for range 3 {
		if err := seedQualityProfiles(ctx, db, cfg, log); err != nil {
			t.Fatal(err)
		}
	}
	if got := profileCount(t, db); got != first {
		t.Errorf("re-seeding created duplicates: %d profiles, want %d", got, first)
	}
	if got := eventCount(t, db); got != eventsAfterFirst {
		t.Errorf("re-seeding emitted %d event(s); a start that changes nothing must emit none",
			got-eventsAfterFirst)
	}
}

// An operator who tunes a default keeps their tuning. A seeder that rewrote it
// every morning would silently revert their work, and they would discover it by
// wondering why the wrong releases keep arriving.
func TestSeedingNeverOverwritesAnEditedProfile(t *testing.T) {
	db, cfg := seedHarness(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)

	if err := seedQualityProfiles(ctx, db, cfg, log); err != nil {
		t.Fatal(err)
	}

	const edited = `[{"attribute":"resolution","op":"gte","value":4320}]`
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx,
			`UPDATE quality_profiles SET accept = ?, description = ? WHERE name = 'living-room'`,
			edited, "mine now")
		return execErr
	})
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if err := seedQualityProfiles(ctx, db, cfg, log); err != nil {
			t.Fatal(err)
		}
	}

	var accept, description string
	if err := db.Reader().QueryRow(
		`SELECT accept, description FROM quality_profiles WHERE name = 'living-room'`,
	).Scan(&accept, &description); err != nil {
		t.Fatal(err)
	}
	if accept != edited {
		t.Errorf("re-seeding reverted an operator's rules:\n got %s\nwant %s", accept, edited)
	}
	if description != "mine now" {
		t.Errorf("re-seeding reverted an operator's description, got %q", description)
	}
}

// A default that does not satisfy its own validator would fail on someone
// else's machine rather than in our tests. Seeding refuses loudly instead of
// writing an unusable row.
func TestSeedingRefusesAnInvalidDefault(t *testing.T) {
	db, cfg := seedHarness(t)
	ctx := context.Background()

	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: cfg.Peer.Name, PeerSite: cfg.Peer.Site,
	})
	if err != nil {
		t.Fatal(err)
	}

	broken := []policy.Profile{{
		Name: "broken",
		// A weight on a gate: the exact category error the domain refuses.
		Accept: []policy.Rule{{
			Attribute: policy.AttrResolution, Op: policy.OpGTE,
			Value: policy.Num(1080), Weight: 10,
		}},
	}}
	if _, err := cat.SeedQualityProfiles(ctx, broken); err == nil {
		t.Fatal("an invalid default must be refused rather than written")
	}
	if got := profileCount(t, db); got != 0 {
		t.Errorf("a refused default must leave no row behind, found %d", got)
	}
}

// A seeded profile that never terminates has to survive the round trip through
// the database. It is the "never stop looking" case, and a nullable column or a
// null-vs-[] confusion is exactly how it would quietly become "terminal with no
// rules, therefore immediately finished".
func TestAnOpenEndedDefaultSurvivesSeeding(t *testing.T) {
	db, cfg := seedHarness(t)
	ctx := context.Background()

	if err := seedQualityProfiles(ctx, db, cfg, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	var openEnded int
	if err := db.Reader().QueryRow(
		`SELECT count(*) FROM quality_profiles WHERE json_array_length(terminal) = 0`,
	).Scan(&openEnded); err != nil {
		t.Fatal(err)
	}
	if openEnded == 0 {
		t.Error("no seeded profile is open-ended, so nothing exercises the " +
			"never-terminal path against a real database")
	}
}
