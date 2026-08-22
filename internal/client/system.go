package client

import (
	"context"
	"net/url"
	"strconv"
)

// The system surface, and the drift check on top of it (#150).
//
// As everywhere else in this package the wire types are declared here rather
// than imported from the server, and the duplication is the point: a field
// renamed on the server must show up as a failing test here rather than
// renaming itself on both sides and agreeing forever. See the header of
// types.go.

// BuildInfo is the running binary's identity.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
}

// PeerInfo identifies this node within the instance (ADR-0010).
type PeerInfo struct {
	Name string `json:"name"`
	Site string `json:"site"`
}

// StorageInfo is one dependency's location and health.
type StorageInfo struct {
	Path string `json:"path"`
	OK   bool   `json:"ok"`
}

// EventsInfo is where the event log currently is. OK is false when the head
// could not be read, in which case Head is meaningless — 0 is a legitimate head
// and resuming from a fabricated one would skip the whole backlog.
type EventsInfo struct {
	Head int64 `json:"head"`
	OK   bool  `json:"ok"`
}

// ToolInfo is one external media binary this node resolved (ADR-0023).
type ToolInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"`
}

// DriftStatus is the outcome of one drift comparison: `current`, `behind`,
// `ahead`, `mismatch`, or `unknown` when no comparison could be made. Unknown
// is not current, and nothing here may treat it as such.
type DriftStatus string

// The drift statuses a comparison can report. See DriftStatus.
const (
	DriftUnknown  DriftStatus = "unknown"
	DriftCurrent  DriftStatus = "current"
	DriftBehind   DriftStatus = "behind"
	DriftAhead    DriftStatus = "ahead"
	DriftMismatch DriftStatus = "mismatch"
)

// BuildIdentity is a build: what it calls itself and what it was built from.
type BuildIdentity struct {
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
}

// BuildDrift is how far the running build is from the expected one. At most one
// of the three distance fields is non-zero — the most significant semantic
// version component that differs — and all three are zero unless Status is
// `behind`.
type BuildDrift struct {
	Status      DriftStatus   `json:"status"`
	Expected    BuildIdentity `json:"expected"`
	Actual      BuildIdentity `json:"actual"`
	MajorBehind int           `json:"major_behind"`
	MinorBehind int           `json:"minor_behind"`
	PatchBehind int           `json:"patch_behind"`
	Detail      string        `json:"detail,omitempty"`
}

// Drifted reports whether the build differs from the expectation at all. An
// unknown comparison is not drift; it is the absence of one.
func (b BuildDrift) Drifted() bool {
	return b.Status == DriftBehind || b.Status == DriftAhead || b.Status == DriftMismatch
}

// SchemaDrift is how many migrations separate the database from the binary.
type SchemaDrift struct {
	Status           DriftStatus `json:"status"`
	Expected         int64       `json:"expected"`
	Applied          int64       `json:"applied"`
	MigrationsBehind int64       `json:"migrations_behind"`
	MigrationsAhead  int64       `json:"migrations_ahead"`
	Detail           string      `json:"detail,omitempty"`
}

// Drifted reports whether the applied schema differs from the expected one.
func (s SchemaDrift) Drifted() bool {
	return s.Status == DriftBehind || s.Status == DriftAhead
}

// DriftReport is both halves, never merged: they drift independently, and a
// current binary with unapplied migrations is its own failure rather than a
// mild case of being behind.
type DriftReport struct {
	Build  BuildDrift  `json:"build"`
	Schema SchemaDrift `json:"schema"`
}

// Drifted reports whether either half drifted.
func (r DriftReport) Drifted() bool { return r.Build.Drifted() || r.Schema.Drifted() }

// SystemInfo is GET /api/v1/system: what this node is, what it is running, how
// far that has drifted from what was expected, and whether its dependencies
// work.
type SystemInfo struct {
	Build         BuildInfo   `json:"build"`
	Peer          PeerInfo    `json:"peer"`
	SchemaVersion int64       `json:"schema_version"`
	Drift         DriftReport `json:"drift"`
	Database      StorageInfo `json:"database"`
	CAS           StorageInfo `json:"cas"`
	Events        EventsInfo  `json:"events"`
	Media         []ToolInfo  `json:"media"`
	AuthEnabled   bool        `json:"auth_enabled"`
}

// Expectation is what a caller believes an instance should be running.
//
// It is optional in both halves and they are independent. An empty Build leaves
// the build comparison unmade, and the endpoint says so with `unknown` rather
// than with `current` — a check that has quietly stopped comparing must not
// look like a fleet that never drifts. A zero Schema means "compare against
// whatever the instance itself embeds", which is the useful default: the node
// already knows the highest migration it ships with.
type Expectation struct {
	Build  BuildIdentity
	Schema int64
}

// query renders the expectation as the request parameters the endpoint reads.
func (e Expectation) query() url.Values {
	q := url.Values{}
	if e.Build.Version != "" {
		q.Set("expected_version", e.Build.Version)
	}
	if e.Build.Commit != "" {
		q.Set("expected_commit", e.Build.Commit)
	}
	if e.Schema > 0 {
		q.Set("expected_schema", strconv.FormatInt(e.Schema, 10))
	}
	return q
}

// System reads GET /api/v1/system, asking it to compare itself against want.
//
// One request, to the instance being asked about and nothing else. That is a
// property worth keeping: a drift check that had to reach a release API or a
// git host would be unavailable in exactly the network conditions that let a
// deployment go stale in the first place.
func (c *Client) System(ctx context.Context, want Expectation) (*SystemInfo, error) {
	var out SystemInfo
	if err := c.Get(ctx, "/system", want.query(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}
