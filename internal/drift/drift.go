// Package drift compares what a running instance IS against what it was
// expected to be, and reports how far behind rather than whether it differs.
//
// It exists because of a deployment that ran 36 commits and seven unapplied
// migrations behind main, across two entire milestones, and nothing anywhere
// would have said so. Every input to the check already existed — the startup
// line carries the version and the commit, the schema version is logged at
// migration — and nothing compared them to anything.
//
// # Why a distance and not a boolean
//
// "Drifted: yes" is the answer that gets muted. Two versions behind on a patch
// release and two milestones behind on the schema are the same boolean and
// nothing like the same problem, so the boolean is read once, judged noisy, and
// filtered out of whatever is watching. A number can be thresholded, graphed
// and alerted on proportionally, and — the reason it matters here — a number
// can be ASSERTED. A test that watches a warning appear proves that something
// printed; a test that watches `migrations_behind` go from 7 to 0 proves what.
//
// # Why build and schema are separate
//
// They drift independently and they fail differently. A binary that is current
// with its migrations unapplied is not a mild case of being behind: it is a
// build running against a schema it was never tested on, which is its own
// failure and often the worse of the two. Reducing both to one "up to date"
// flag would let a current binary hide an un-migrated database.
//
// So there are two comparisons, two functions and two results, and Report
// carries both without combining them.
//
// # What this package does not do
//
// It does not fetch anything. Comparison is pure: callers supply the expected
// identity and the observed one, which is what lets the check run with no
// network access to anything but the instance itself.
package drift

import (
	"fmt"
	"strconv"
	"strings"
)

// Status is the outcome of one comparison. It is an enum rather than free text
// because the first consumer of this is a machine.
type Status string

const (
	// StatusUnknown means the comparison could not be made — nothing to compare
	// against, or a version string that is not a version. It is deliberately
	// distinct from StatusCurrent: "we could not tell" reported as "fine" is
	// exactly the failure this package exists to stop.
	StatusUnknown Status = "unknown"
	// StatusCurrent means the running instance is what was expected.
	StatusCurrent Status = "current"
	// StatusBehind means the running instance is older than expected, and the
	// distance fields say by how much.
	StatusBehind Status = "behind"
	// StatusAhead means the running instance is NEWER than expected. Not a
	// pleasant surprise: for the schema it is the state that makes an old
	// binary write plausible rows against constraints it does not know about
	// (see sqlite.ErrSchemaNewerThanBinary), and for the build it usually means
	// the expectation is the stale thing.
	StatusAhead Status = "ahead"
	// StatusMismatch means the two builds are different builds, and no ordering
	// between them can be established — same version tag, different commit, or
	// two commits with no parseable version on either side. Different, distance
	// unknown; not "current".
	StatusMismatch Status = "mismatch"
)

// Identity is a build: what it calls itself, and what it was built from.
type Identity struct {
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
}

// empty reports whether this identity says nothing at all.
func (i Identity) empty() bool { return i.Version == "" && i.Commit == "" }

// Build is the result of comparing an expected build against a running one.
//
// The three distance fields are the "how far behind" for the build half. At
// most one of them is non-zero: the most significant semantic-version component
// that differs, because a distance spread over three numbers is a distance
// nobody can threshold. v1.4.2 expected against v1.0.9 running is one minor
// version behind and says so, rather than claiming to be four minors and minus
// seven patches behind.
//
// They are all zero whenever Status is anything but StatusBehind — including
// StatusMismatch, where the builds genuinely differ and there is no honest
// number to report.
type Build struct {
	Status      Status   `json:"status"`
	Expected    Identity `json:"expected"`
	Actual      Identity `json:"actual"`
	MajorBehind int      `json:"major_behind"`
	MinorBehind int      `json:"minor_behind"`
	PatchBehind int      `json:"patch_behind"`
	// Detail explains a status that a number cannot, in a few words. It is for
	// a human reading the output; nothing should branch on it.
	Detail string `json:"detail,omitempty"`
}

// Drifted reports whether this build is anything other than the expected one.
// An unknown comparison is NOT drift — it is the absence of a comparison, and
// treating it as drift would make every un-stamped dev build alarm.
func (b Build) Drifted() bool {
	return b.Status == StatusBehind || b.Status == StatusAhead || b.Status == StatusMismatch
}

// Schema is the result of comparing the expected schema version against the one
// actually applied to the database.
//
// MigrationsBehind is the count of migrations that exist and have not run. It
// is the number the deployment in #132 would have reported as 7.
// MigrationsAhead is the reverse, and at most one of the two is non-zero.
type Schema struct {
	Status           Status `json:"status"`
	Expected         int64  `json:"expected"`
	Applied          int64  `json:"applied"`
	MigrationsBehind int64  `json:"migrations_behind"`
	MigrationsAhead  int64  `json:"migrations_ahead"`
	Detail           string `json:"detail,omitempty"`
}

// Drifted reports whether the applied schema is anything but the expected one.
func (s Schema) Drifted() bool {
	return s.Status == StatusBehind || s.Status == StatusAhead
}

// Report is both checks, side by side and never merged. See the package comment
// for why they are not reduced to one answer.
type Report struct {
	Build  Build  `json:"build"`
	Schema Schema `json:"schema"`
}

// Drifted reports whether EITHER half drifted.
func (r Report) Drifted() bool { return r.Build.Drifted() || r.Schema.Drifted() }

// CompareBuild compares an expected build identity against a running one.
//
// Ordering comes from the semantic version when both sides carry one. When
// either does not — `dev`, or the `git describe` stamp a source build gets —
// there is no ordering to derive, so the commits decide whether the builds are
// the same build, and the result is StatusMismatch rather than a guessed
// distance. Reporting "different, distance unknown" is the honest answer and
// still catches the case this package was written for: a host pinned to a
// commit nobody has looked at in two milestones.
func CompareBuild(expected, actual Identity) Build {
	b := Build{Status: StatusUnknown, Expected: expected, Actual: actual}
	if expected.empty() {
		b.Detail = "no expected build was supplied, so nothing was compared"
		return b
	}
	if actual.empty() {
		b.Detail = "the instance reports no build identity"
		return b
	}

	wantV, wantOK := parseVersion(expected.Version)
	gotV, gotOK := parseVersion(actual.Version)
	if wantOK && gotOK {
		switch cmp := wantV.compare(gotV); {
		case cmp > 0:
			b.Status = StatusBehind
			b.MajorBehind, b.MinorBehind, b.PatchBehind = wantV.behind(gotV)
		case cmp < 0:
			b.Status = StatusAhead
			b.Detail = "the instance is newer than expected; the expectation is probably the stale half"
		default:
			b.Status, b.Detail = sameVersion(expected, actual)
		}
		return b
	}

	switch {
	case expected.Commit != "" && actual.Commit != "":
		if commitsAgree(expected.Commit, actual.Commit) {
			b.Status = StatusCurrent
			return b
		}
		b.Status = StatusMismatch
		b.Detail = "the commits differ and neither side carries a semantic version, " +
			"so the builds are known to differ but not by how much"
	case expected.Version != "" && expected.Version == actual.Version:
		b.Status = StatusCurrent
	default:
		b.Detail = fmt.Sprintf("cannot compare %q against %q: neither a semantic version "+
			"on both sides nor a commit on both sides", expected.Version, actual.Version)
	}
	return b
}

// sameVersion decides between current and mismatch for two builds whose
// semantic versions are equal.
//
// Equal versions with different commits is not a curiosity: it is what a
// re-cut tag, a dirty build or a hand-copied binary looks like, and calling it
// "current" is how a machine running something nobody can identify passes a
// version check.
func sameVersion(expected, actual Identity) (Status, string) {
	if expected.Commit != "" && actual.Commit != "" && !commitsAgree(expected.Commit, actual.Commit) {
		return StatusMismatch, "the versions agree and the commits do not: " +
			"the same version tag was built twice from different source"
	}
	return StatusCurrent, ""
}

// minCommitPrefix is how much of a commit hash must be present before a prefix
// match is allowed to mean anything. Git's own abbreviation floor is 7, and
// below that a match is a coincidence rather than evidence.
const minCommitPrefix = 7

// commitsAgree reports whether two commit stamps name the same commit.
//
// One side is routinely abbreviated and the other is not — the Makefile stamps
// `git rev-parse --short`, a release pipeline or a human stamps the full forty
// — so an equality check would report every correctly-deployed host as a
// mismatch. Prefix matching, with a floor, is what makes the comparison usable
// against the stamps that actually exist.
func commitsAgree(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return true
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	if len(short) < minCommitPrefix {
		return false
	}
	return strings.HasPrefix(long, short)
}

// version is a parsed major.minor.patch triple.
type version struct{ major, minor, patch int }

func (v version) compare(o version) int {
	switch {
	case v.major != o.major:
		return sign(v.major - o.major)
	case v.minor != o.minor:
		return sign(v.minor - o.minor)
	default:
		return sign(v.patch - o.patch)
	}
}

// behind returns how far o is behind v, in the most significant component that
// differs. It is only meaningful when v.compare(o) > 0, which is the only place
// it is called from.
func (v version) behind(o version) (major, minor, patch int) {
	switch {
	case v.major != o.major:
		return v.major - o.major, 0, 0
	case v.minor != o.minor:
		return 0, v.minor - o.minor, 0
	default:
		return 0, 0, v.patch - o.patch
	}
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	default:
		return 0
	}
}

// parseVersion reads `v1.2.3`, `1.2.3`, `v1.2.3-rc1` and `1.2.3+meta`.
//
// The pre-release and build-metadata suffixes are DROPPED rather than ordered.
// Semver's pre-release ordering is real and this does not implement it, so
// rather than half-implement it and be confidently wrong about which of two
// release candidates is newer, `1.2.3-rc1` and `1.2.3` compare equal here and
// the commit decides whether they are the same build. That is the same answer
// this function gives for any other pair it cannot order, which keeps the
// failure mode uniform.
func parseVersion(s string) (version, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return version{}, false
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	s, _, _ = strings.Cut(s, "+")
	s, _, _ = strings.Cut(s, "-")

	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, false
		}
		out[i] = n
	}
	return version{major: out[0], minor: out[1], patch: out[2]}, true
}

// CompareSchema compares the schema version a binary knows about against the
// one actually applied to its database.
//
// A zero or negative expectation is not "version zero": it is a caller that
// does not know what to expect, and reporting that as "the database is ahead"
// would be a fabricated alarm. Applied MAY legitimately be zero — a database
// that has never been migrated is at version 0 — and is reported as behind by
// the full distance, which is correct.
func CompareSchema(expected, applied int64) Schema {
	s := Schema{Status: StatusUnknown, Expected: expected, Applied: applied}
	if expected <= 0 {
		s.Detail = "no expected schema version was supplied, so nothing was compared"
		return s
	}
	switch {
	case applied < expected:
		s.Status = StatusBehind
		s.MigrationsBehind = expected - applied
	case applied > expected:
		s.Status = StatusAhead
		s.MigrationsAhead = applied - expected
		s.Detail = "the database was migrated by a newer build; an older binary writing " +
			"against a newer schema corrupts it silently (§49)"
	default:
		s.Status = StatusCurrent
	}
	return s
}
