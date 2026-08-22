package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// `heyarr system drift` over a real API and a real database (#150).
//
// The discipline every test here follows, and the reason the issue exists:
//
//	NEVER ASSERT ON AN ABSENCE WITHOUT FIRST PROVING THE MECHANISM EXISTS.
//
// #132's verification asked for the SILENCE of a warning that had landed after
// the build being verified. The host contained zero occurrences of it, so the
// silence was total and meant nothing, and asserting on it would have passed.
// Every "no drift" assertion below therefore runs AFTER the same command has
// been watched to fire on the same harness.
//
// The harness reports build `test`/`abc123` at applied schema 4, so the
// expectation is passed explicitly rather than defaulted: a golden file whose
// contents move every time a migration lands is a golden file people stop
// reading.

// harnessExpectation is the instance the harness actually is.
var harnessExpectation = []string{"--expected-version", "test", "--expected-commit", "abc123"}

func driftArgs(extra ...string) []string {
	return append(append([]string{"system", "drift"}, harnessExpectation...), extra...)
}

// TestSystemDriftFiresAndThenGoesQuiet is the A/B, on the schema half, with the
// numbers from #132: eleven expected against four applied is seven migrations.
func TestSystemDriftFiresAndThenGoesQuiet(t *testing.T) {
	h := newAPIHarness(t)

	// A: the drift case. It must be visible in the output AND in the exit code.
	out, _, err := h.run(driftArgs("--expected-schema", "11", "--json")...)
	if err == nil {
		t.Fatalf("system drift exited 0 against an instance seven migrations behind:\n%s", out)
	}
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("error = %v, want ErrDrift", err)
	}
	var fired client.DriftReport
	if jsonErr := json.Unmarshal([]byte(out), &fired); jsonErr != nil {
		t.Fatalf("decoding --json output: %v\n%s", jsonErr, out)
	}
	if fired.Schema.Status != client.DriftBehind {
		t.Fatalf("schema status = %q, want %q — the drift case did not fire, so the "+
			"silence asserted below would prove nothing", fired.Schema.Status, client.DriftBehind)
	}
	if fired.Schema.MigrationsBehind != 7 {
		t.Fatalf("migrations_behind = %d, want 7", fired.Schema.MigrationsBehind)
	}

	// B: and only now, the silence. Same command, same harness, an expectation
	// the instance meets.
	out, _, err = h.run(driftArgs("--expected-schema", "4", "--json")...)
	if err != nil {
		t.Fatalf("system drift failed against an instance that matches: %v\n%s", err, out)
	}
	var quiet client.DriftReport
	if jsonErr := json.Unmarshal([]byte(out), &quiet); jsonErr != nil {
		t.Fatalf("decoding --json output: %v\n%s", jsonErr, out)
	}
	if quiet.Schema.Status != client.DriftCurrent {
		t.Errorf("schema status = %q, want %q", quiet.Schema.Status, client.DriftCurrent)
	}
	if quiet.Build.Status != client.DriftCurrent {
		t.Errorf("build status = %q, want %q", quiet.Build.Status, client.DriftCurrent)
	}
	if quiet.Drifted() {
		t.Errorf("the report says drift on an instance that matches: %+v", quiet)
	}
}

// The two halves are reported independently, in both directions. A current
// binary with unapplied migrations is its own failure, not a mild case of being
// behind, and neither half may be hidden by the other.
func TestSystemDriftSeparatesBuildFromSchema(t *testing.T) {
	h := newAPIHarness(t)

	t.Run("a current build with an old schema", func(t *testing.T) {
		out, _, err := h.run(driftArgs("--expected-schema", "11", "--json")...)
		if err == nil {
			t.Fatal("system drift exited 0 with seven migrations unapplied")
		}
		got := decodeDrift(t, out)
		if got.Build.Status != client.DriftCurrent {
			t.Errorf("build status = %q, want %q", got.Build.Status, client.DriftCurrent)
		}
		if got.Schema.Status != client.DriftBehind || got.Schema.MigrationsBehind != 7 {
			t.Errorf("schema = %+v, want behind by 7", got.Schema)
		}
	})

	t.Run("an old build with a current schema", func(t *testing.T) {
		out, _, err := h.run("system", "drift",
			"--expected-version", "v9.9.9", "--expected-commit", "deadbeefdeadbeef",
			"--expected-schema", "4", "--json")
		if err == nil {
			t.Fatal("system drift exited 0 against a build that is not the expected one")
		}
		got := decodeDrift(t, out)
		if got.Schema.Status != client.DriftCurrent {
			t.Errorf("schema status = %q, want %q", got.Schema.Status, client.DriftCurrent)
		}
		// `test` is not a semantic version, so there is no distance to report —
		// but the builds are known to differ, and "mismatch" says exactly that
		// rather than the "current" a naive equality check would produce.
		if got.Build.Status != client.DriftMismatch {
			t.Errorf("build status = %q, want %q (detail: %s)",
				got.Build.Status, client.DriftMismatch, got.Build.Detail)
		}
	})
}

// An expectation the command cannot order must report "unknown", never
// "current". This is #132's failure mode stated as a test: a check that has
// stopped comparing looks exactly like a fleet that never drifts.
func TestSystemDriftReportsUnknownRatherThanCurrent(t *testing.T) {
	h := newAPIHarness(t)
	out, _, err := h.run("system", "drift",
		"--expected-version", "nightly", "--expected-schema", "4", "--json")
	if err != nil {
		t.Fatalf("system drift failed: %v\n%s", err, out)
	}
	got := decodeDrift(t, out)
	if got.Build.Status != client.DriftUnknown {
		t.Errorf("build status = %q, want %q", got.Build.Status, client.DriftUnknown)
	}
	if got.Build.Detail == "" {
		t.Error("an unknown comparison says nothing about why")
	}
	if got.Build.Drifted() {
		t.Error("an unmade comparison was counted as drift")
	}
}

// The human output has to carry the number too. A table that says "behind" and
// makes you reach for --json to find out by how much is the boolean this issue
// was written to avoid.
func TestSystemDriftHumanOutputCarriesTheDistance(t *testing.T) {
	h := newAPIHarness(t)
	out, _, err := h.run(driftArgs("--expected-schema", "11")...)
	if err == nil {
		t.Fatalf("system drift exited 0 against an instance seven migrations behind:\n%s", out)
	}
	if !strings.Contains(out, "7 migrations behind") {
		t.Errorf("the human output does not say how far behind:\n%s", out)
	}
}

func TestSystemDriftJSONShape(t *testing.T) {
	h := newAPIHarness(t)
	out, _, err := h.run(driftArgs("--expected-schema", "11", "--json")...)
	if err == nil {
		t.Fatalf("system drift exited 0 with drift present:\n%s", out)
	}
	testutil.Golden(t, "testdata/system_drift.json", []byte(normalise(out)))
}

func TestSystemDriftCurrentJSONShape(t *testing.T) {
	h := newAPIHarness(t)
	out := h.mustRun(driftArgs("--expected-schema", "4", "--json")...)
	testutil.Golden(t, "testdata/system_drift_current.json", []byte(normalise(out)))
}

func TestSystemInfoJSONShape(t *testing.T) {
	h := newAPIHarness(t)
	out := h.mustRun("system", "info", "--json")
	testutil.Golden(t, "testdata/system_info.json", []byte(h.normalisePaths(normalise(out))))
}

func TestSystemInfoHumanOutput(t *testing.T) {
	h := newAPIHarness(t)
	out := h.mustRun("system", "info")
	for _, want := range []string{"heyarr test (abc123)", "schema version  4", "schema drift"} {
		if !strings.Contains(out, want) {
			t.Errorf("system info does not report %q:\n%s", want, out)
		}
	}
}

func decodeDrift(t *testing.T, out string) client.DriftReport {
	t.Helper()
	var r client.DriftReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("decoding --json output: %v\n%s", err, out)
	}
	return r
}
