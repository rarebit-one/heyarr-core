package drift_test

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/drift"
)

// The rule this file exists to encode, from #150:
//
//	NEVER ASSERT ON AN ABSENCE WITHOUT FIRST PROVING THE MECHANISM EXISTS.
//
// #132 found it the expensive way. A verification procedure asked for the
// SILENCE of a warning as proof that a problem was gone — and the warning had
// landed after the build running on the host, so the host contained zero
// occurrences of it. The silence was total, and it meant nothing. Asserting on
// it would have passed.
//
// So every "no drift" assertion below is preceded, in the same test function
// and against the same comparison, by an assertion that watches the SAME check
// fire on drifted input. A silence is only evidence once the thing that would
// have broken it has been seen to work.

// TestBuildDriftFiresAndThenGoesQuiet is the A/B for the build half.
func TestBuildDriftFiresAndThenGoesQuiet(t *testing.T) {
	// A: the drift case. The expectation is two minor versions ahead of what is
	// running, and the check must say so with the number.
	fired := drift.CompareBuild(
		drift.Identity{Version: "v1.4.0", Commit: "aaaaaaaaaaaa"},
		drift.Identity{Version: "v1.2.0", Commit: "bbbbbbbbbbbb"},
	)
	if fired.Status != drift.StatusBehind {
		t.Fatalf("status = %q, want %q — the drift case did not fire, so the silence "+
			"asserted below would prove nothing", fired.Status, drift.StatusBehind)
	}
	if fired.MinorBehind != 2 {
		t.Fatalf("minor_behind = %d, want 2", fired.MinorBehind)
	}
	if !fired.Drifted() {
		t.Fatal("Drifted() is false on a build that is two minor versions behind")
	}

	// B: and only now, the silence. Same function, same fields, an instance at
	// the expected build.
	quiet := drift.CompareBuild(
		drift.Identity{Version: "v1.4.0", Commit: "aaaaaaaaaaaa"},
		drift.Identity{Version: "v1.4.0", Commit: "aaaaaaaaaaaa"},
	)
	if quiet.Status != drift.StatusCurrent {
		t.Errorf("status = %q, want %q", quiet.Status, drift.StatusCurrent)
	}
	if quiet.MajorBehind != 0 || quiet.MinorBehind != 0 || quiet.PatchBehind != 0 {
		t.Errorf("a current build reports a distance: %+v", quiet)
	}
	if quiet.Drifted() {
		t.Error("Drifted() is true on a build that matches the expectation")
	}
}

// TestSchemaDriftFiresAndThenGoesQuiet is the A/B for the schema half, on the
// exact numbers from #132: a database at version 7 under a binary that knows 14
// is seven migrations behind.
func TestSchemaDriftFiresAndThenGoesQuiet(t *testing.T) {
	fired := drift.CompareSchema(14, 7)
	if fired.Status != drift.StatusBehind {
		t.Fatalf("status = %q, want %q — the drift case did not fire", fired.Status, drift.StatusBehind)
	}
	if fired.MigrationsBehind != 7 {
		t.Fatalf("migrations_behind = %d, want 7", fired.MigrationsBehind)
	}
	if fired.MigrationsAhead != 0 {
		t.Errorf("migrations_ahead = %d on a database that is behind", fired.MigrationsAhead)
	}

	quiet := drift.CompareSchema(14, 14)
	if quiet.Status != drift.StatusCurrent {
		t.Errorf("status = %q, want %q", quiet.Status, drift.StatusCurrent)
	}
	if quiet.MigrationsBehind != 0 || quiet.MigrationsAhead != 0 {
		t.Errorf("a current schema reports a distance: %+v", quiet)
	}
	if quiet.Drifted() {
		t.Error("Drifted() is true on a schema that matches the expectation")
	}
}

// TestTheTwoHalvesDriftIndependently is the assertion that keeps them from
// being collapsed into one flag. Both directions, because a current binary with
// unapplied migrations and a stale binary against a current database are
// different failures and neither may be hidden by the other.
func TestTheTwoHalvesDriftIndependently(t *testing.T) {
	current := drift.Identity{Version: "v2.0.0", Commit: "cafebabecafe"}
	stale := drift.Identity{Version: "v1.0.0", Commit: "0123456789ab"}

	t.Run("a current binary with an old schema", func(t *testing.T) {
		r := drift.Report{
			Build:  drift.CompareBuild(current, current),
			Schema: drift.CompareSchema(18, 11),
		}
		if r.Build.Status != drift.StatusCurrent {
			t.Errorf("build status = %q, want %q", r.Build.Status, drift.StatusCurrent)
		}
		if r.Schema.Status != drift.StatusBehind {
			t.Errorf("schema status = %q, want %q", r.Schema.Status, drift.StatusBehind)
		}
		if r.Schema.MigrationsBehind != 7 {
			t.Errorf("migrations_behind = %d, want 7", r.Schema.MigrationsBehind)
		}
		if !r.Drifted() {
			t.Error("the report says nothing drifted while seven migrations are unapplied")
		}
	})

	t.Run("an old binary with a current schema", func(t *testing.T) {
		r := drift.Report{
			Build:  drift.CompareBuild(current, stale),
			Schema: drift.CompareSchema(18, 18),
		}
		if r.Build.Status != drift.StatusBehind {
			t.Errorf("build status = %q, want %q", r.Build.Status, drift.StatusBehind)
		}
		if r.Build.MajorBehind != 1 {
			t.Errorf("major_behind = %d, want 1", r.Build.MajorBehind)
		}
		if r.Schema.Status != drift.StatusCurrent {
			t.Errorf("schema status = %q, want %q", r.Schema.Status, drift.StatusCurrent)
		}
		if r.Schema.Drifted() {
			t.Error("the schema half reported drift because the build half did")
		}
		if !r.Drifted() {
			t.Error("the report says nothing drifted while the binary is a major version behind")
		}
	})
}

func TestCompareBuild(t *testing.T) {
	tests := []struct {
		name                string
		expected, actual    drift.Identity
		status              drift.Status
		major, minor, patch int
		wantDrifted         bool
	}{
		{
			name:     "a major version behind",
			expected: drift.Identity{Version: "v3.1.4"},
			actual:   drift.Identity{Version: "v1.9.9"},
			status:   drift.StatusBehind, major: 2, wantDrifted: true,
		},
		{
			name:     "a minor version behind",
			expected: drift.Identity{Version: "1.4.2"},
			actual:   drift.Identity{Version: "1.0.9"},
			status:   drift.StatusBehind, minor: 4, wantDrifted: true,
		},
		{
			name:     "patches behind",
			expected: drift.Identity{Version: "v0.2.11"},
			actual:   drift.Identity{Version: "v0.2.3"},
			status:   drift.StatusBehind, patch: 8, wantDrifted: true,
		},
		{
			name:     "the leading v is optional on either side",
			expected: drift.Identity{Version: "v1.2.0"},
			actual:   drift.Identity{Version: "1.2.0"},
			status:   drift.StatusCurrent,
		},
		{
			name:     "ahead of the expectation",
			expected: drift.Identity{Version: "v1.0.0"},
			actual:   drift.Identity{Version: "v1.1.0"},
			status:   drift.StatusAhead, wantDrifted: true,
		},
		{
			name:     "the same tag built from different source",
			expected: drift.Identity{Version: "v1.2.3", Commit: "1111111111111111"},
			actual:   drift.Identity{Version: "v1.2.3", Commit: "2222222222222222"},
			status:   drift.StatusMismatch, wantDrifted: true,
		},
		{
			name:     "an abbreviated commit still matches its full form",
			expected: drift.Identity{Version: "v1.2.3", Commit: "324a0fc1e2d3a4b5c6d7"},
			actual:   drift.Identity{Version: "v1.2.3", Commit: "324a0fc"},
			status:   drift.StatusCurrent,
		},
		{
			name:     "a commit too short to mean anything is not a match",
			expected: drift.Identity{Version: "v1.2.3", Commit: "324a0fc1e2d3"},
			actual:   drift.Identity{Version: "v1.2.3", Commit: "324a0"},
			status:   drift.StatusMismatch, wantDrifted: true,
		},
		{
			name:     "unstamped source builds compared by commit alone",
			expected: drift.Identity{Version: "dev", Commit: "950ec9d0000000"},
			actual:   drift.Identity{Version: "dev", Commit: "324a0fc0000000"},
			status:   drift.StatusMismatch, wantDrifted: true,
		},
		{
			name:     "unstamped source builds at the same commit",
			expected: drift.Identity{Version: "32f5a4f", Commit: "32f5a4f1234567"},
			actual:   drift.Identity{Version: "32f5a4f", Commit: "32f5a4f1234567"},
			status:   drift.StatusCurrent,
		},
		{
			name:     "a pre-release orders as its release, and the commit settles it",
			expected: drift.Identity{Version: "v1.2.3-rc1", Commit: "abcdef0123456"},
			actual:   drift.Identity{Version: "v1.2.3", Commit: "abcdef0123456"},
			status:   drift.StatusCurrent,
		},
		{
			name:     "no expectation is unknown, never current",
			expected: drift.Identity{},
			actual:   drift.Identity{Version: "v1.2.3", Commit: "abcdef0123456"},
			status:   drift.StatusUnknown,
		},
		{
			name:     "an instance that reports nothing is unknown, never current",
			expected: drift.Identity{Version: "v1.2.3", Commit: "abcdef0123456"},
			actual:   drift.Identity{},
			status:   drift.StatusUnknown,
		},
		{
			name:     "uncomparable versions with no commits are unknown, never current",
			expected: drift.Identity{Version: "dev"},
			actual:   drift.Identity{Version: "32f5a4f"},
			status:   drift.StatusUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := drift.CompareBuild(tt.expected, tt.actual)
			if got.Status != tt.status {
				t.Errorf("status = %q, want %q (detail: %s)", got.Status, tt.status, got.Detail)
			}
			if got.MajorBehind != tt.major || got.MinorBehind != tt.minor || got.PatchBehind != tt.patch {
				t.Errorf("distance = %d.%d.%d, want %d.%d.%d",
					got.MajorBehind, got.MinorBehind, got.PatchBehind, tt.major, tt.minor, tt.patch)
			}
			if got.Drifted() != tt.wantDrifted {
				t.Errorf("Drifted() = %v, want %v", got.Drifted(), tt.wantDrifted)
			}
			// The comparison must carry both sides back out with it. A report
			// that says "behind" without saying behind WHAT is a report nobody
			// can act on without going and looking.
			if got.Expected != tt.expected || got.Actual != tt.actual {
				t.Errorf("the report lost its inputs: %+v", got)
			}
		})
	}
}

func TestCompareSchema(t *testing.T) {
	tests := []struct {
		name              string
		expected, applied int64
		status            drift.Status
		behind, ahead     int64
		wantDrifted       bool
	}{
		{
			name: "the deployment in #132", expected: 18, applied: 11,
			status: drift.StatusBehind, behind: 7, wantDrifted: true,
		},
		{
			name: "a database that has never been migrated", expected: 18, applied: 0,
			status: drift.StatusBehind, behind: 18, wantDrifted: true,
		},
		{
			name: "up to date", expected: 18, applied: 18,
			status: drift.StatusCurrent,
		},
		{
			name: "migrated by a newer build", expected: 11, applied: 18,
			status: drift.StatusAhead, ahead: 7, wantDrifted: true,
		},
		{
			name: "no expectation is unknown, never current", expected: 0, applied: 18,
			status: drift.StatusUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := drift.CompareSchema(tt.expected, tt.applied)
			if got.Status != tt.status {
				t.Errorf("status = %q, want %q (detail: %s)", got.Status, tt.status, got.Detail)
			}
			if got.MigrationsBehind != tt.behind {
				t.Errorf("migrations_behind = %d, want %d", got.MigrationsBehind, tt.behind)
			}
			if got.MigrationsAhead != tt.ahead {
				t.Errorf("migrations_ahead = %d, want %d", got.MigrationsAhead, tt.ahead)
			}
			if got.Drifted() != tt.wantDrifted {
				t.Errorf("Drifted() = %v, want %v", got.Drifted(), tt.wantDrifted)
			}
			if got.Expected != tt.expected || got.Applied != tt.applied {
				t.Errorf("the report lost its inputs: %+v", got)
			}
		})
	}
}

// TestUnknownIsNeverMistakenForCurrent is the failure mode of #132 stated
// directly. Every input that cannot be compared must land on "unknown", because
// "we could not tell" rendered as "fine" is precisely how a host runs two
// milestones behind while something reports it healthy.
func TestUnknownIsNeverMistakenForCurrent(t *testing.T) {
	uncomparable := []drift.Build{
		drift.CompareBuild(drift.Identity{}, drift.Identity{Version: "v1.0.0"}),
		drift.CompareBuild(drift.Identity{Version: "v1.0.0"}, drift.Identity{}),
		drift.CompareBuild(drift.Identity{Version: "dev"}, drift.Identity{Version: "nightly"}),
	}
	for _, b := range uncomparable {
		if b.Status == drift.StatusCurrent {
			t.Errorf("an uncomparable build reported itself current: %+v", b)
		}
		if b.Status != drift.StatusUnknown {
			t.Errorf("status = %q, want %q: %+v", b.Status, drift.StatusUnknown, b)
		}
		if b.Detail == "" {
			t.Errorf("an unknown status says nothing about why: %+v", b)
		}
	}

	s := drift.CompareSchema(0, 18)
	if s.Status != drift.StatusUnknown {
		t.Errorf("schema status = %q, want %q", s.Status, drift.StatusUnknown)
	}
	if s.Detail == "" {
		t.Error("an unknown schema status says nothing about why")
	}
}
