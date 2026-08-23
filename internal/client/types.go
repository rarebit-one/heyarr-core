package client

import (
	"encoding/json"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// The wire types, from the client's side.
//
// They are declared here rather than imported from internal/api/resources on
// purpose, and the duplication is the point. If the client shared the server's
// structs, a field renamed on the server would rename itself on the client and
// the two would agree forever — including about a change that breaks every
// other consumer of the API. Two independent declarations mean a breaking
// change shows up as a failing test here (see TestWireTypesMatchTheServer,
// which decodes the API's own golden files into these structs with unknown
// fields rejected).
//
// Every timestamp is RFC 3339 in UTC (ADR-0017).

// Work is a semantic unit of content (§11).
type Work struct {
	ID          string          `json:"id"`
	ContentType string          `json:"content_type"`
	WorkKey     string          `json:"work_key"`
	Title       string          `json:"title"`
	SortTitle   string          `json:"sort_title"`
	Year        *int64          `json:"year"`
	Attributes  json.RawMessage `json:"attributes"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Edition is one concrete form of a Work.
type Edition struct {
	ID          string          `json:"id"`
	WorkID      string          `json:"work_id"`
	Label       string          `json:"label"`
	EditionType string          `json:"edition_type"`
	Language    *string         `json:"language"`
	Attributes  json.RawMessage `json:"attributes"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Asset is a file belonging to an Edition. BlobHash is null for a `linked`
// asset, which has no blob at all (ADR-0020).
type Asset struct {
	ID                   string     `json:"id"`
	EditionID            string     `json:"edition_id"`
	LibraryID            *string    `json:"library_id"`
	SourceClass          string     `json:"source_class"`
	BlobHash             *string    `json:"blob_hash"`
	SourcePath           *string    `json:"source_path"`
	Role                 string     `json:"role"`
	Filename             *string    `json:"filename"`
	MIME                 *string    `json:"mime"`
	IdentificationSource string     `json:"identification_source"`
	MissingSince         *time.Time `json:"missing_since"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// Blob is byte identity and nothing else (ADR-0005).
type Blob struct {
	Hash string  `json:"hash"`
	Size int64   `json:"size"`
	MIME *string `json:"mime"`
	// Chunked is the compatibility field: true only when a manifest is
	// present. It cannot express §16's third state — read ChunkManifest.
	Chunked bool `json:"chunked"`
	// ChunkManifest is "present", "not_required" or "undecided" (ADR-0034).
	ChunkManifest manifests.State `json:"chunk_manifest"`
	FirstSeenAt   time.Time       `json:"first_seen_at"`
}

// Library is a configured collection of roots.
type Library struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	ContentType string        `json:"content_type"`
	Enabled     bool          `json:"enabled"`
	CreatedAt   time.Time     `json:"created_at"`
	Roots       []LibraryRoot `json:"roots"`
}

// LibraryRoot is one directory a library is scanned from.
type LibraryRoot struct {
	ID         string    `json:"id"`
	LibraryID  string    `json:"library_id"`
	Path       string    `json:"path"`
	IngestMode string    `json:"ingest_mode"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
}

// Peer is a node in the instance (ADR-0010).
type Peer struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Site     string  `json:"site"`
	Mode     string  `json:"mode"`
	Endpoint *string `json:"endpoint"`
	// PublicKey is the peer's Ed25519 identity as "ed25519:<hex>", or nil for
	// a peer that has not established one yet (ADR-0012, M4-03).
	PublicKey *string   `json:"public_key"`
	IsSelf    bool      `json:"is_self"`
	CreatedAt time.Time `json:"created_at"`
	// EnrolledAt is when this peer was admitted to the fabric (M4-04).
	EnrolledAt time.Time `json:"enrolled_at"`
	// Health is reachability — unknown, reachable or unreachable (§31, M4-10).
	// It is observed from interactions rather than declared by the peer, and
	// unreachable means silence past the window rather than a failed request.
	Health string `json:"health"`
	// LastSeenAt is when the peer last answered anything, or nil if it never
	// has. Health without it is a status nobody can act on.
	LastSeenAt *time.Time `json:"last_seen_at"`
	// Snapshot is the peer's materialised catalog snapshot (§52, M4-13), or
	// nil when the controller has never issued it one. Nil is "no snapshot",
	// which is a different answer from a snapshot of an empty library.
	Snapshot *PeerSnapshot `json:"snapshot"`
}

// PeerSnapshot is a peer's catalog snapshot: which controller, which version,
// and how old (§52, §53).
type PeerSnapshot struct {
	ControllerID  string    `json:"controller_id"`
	Version       int64     `json:"version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Kind          string    `json:"kind"`
	Rows          int64     `json:"rows"`
	ContentDigest string    `json:"content_digest"`
	// AgeSeconds is measured against the CONTROLLER's clock, which is the only
	// clock that can say how stale its own catalogue read is.
	AgeSeconds float64 `json:"age_seconds"`
}

// Replica is one peer's holding of one blob (§8).
type Replica struct {
	BlobHash     string     `json:"blob_hash"`
	PeerID       string     `json:"peer_id"`
	State        string     `json:"state"`
	BytesPresent int64      `json:"bytes_present"`
	VerifiedAt   *time.Time `json:"verified_at"`
	// ReportedAt is when the holding peer last confirmed this row in an
	// inventory report — null if no peer ever has. Distinct from VerifiedAt,
	// which is when the bytes were last re-hashed (M4-07).
	ReportedAt *time.Time `json:"reported_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// JobState is where a job is in its life. The two that end a wait are
// JobSucceeded and JobDead; JobFailed is an attempt that will be retried, and
// treating it as terminal is how `--wait` learns to report failure for work
// that then quietly succeeds.
type JobState string

// The job states (§75).
const (
	JobPending   JobState = "pending"
	JobLeased    JobState = "leased"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobDead      JobState = "dead"
)

// Terminal reports whether a job has stopped for good.
func (s JobState) Terminal() bool { return s == JobSucceeded || s == JobDead }

// Job is one unit of durable work (§75).
type Job struct {
	ID                 string          `json:"id"`
	Type               string          `json:"type"`
	Payload            json.RawMessage `json:"payload"`
	State              JobState        `json:"state"`
	Priority           int             `json:"priority"`
	DedupeKey          *string         `json:"dedupe_key"`
	RequiredCapability string          `json:"required_capability"`
	RunAfter           time.Time       `json:"run_after"`
	Attempts           int             `json:"attempts"`
	MaxAttempts        int             `json:"max_attempts"`
	LeaseOwner         *string         `json:"lease_owner"`
	LeaseExpiresAt     *time.Time      `json:"lease_expires_at"`
	LastError          *string         `json:"last_error"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	FinishedAt         *time.Time      `json:"finished_at"`
}

// ScanResponse is what POST /libraries/{id}/scan returns. It is a list because
// a scan is enqueued per root, and a library may have several — a single job id
// would be a lie for every library with two roots, and `--wait` would exit 0
// while half the scan was still running.
type ScanResponse struct {
	LibraryID string `json:"library_id"`
	Jobs      []Job  `json:"jobs"`
}

// Event is one recorded state transition (§76).
type Event struct {
	Seq         int64           `json:"seq"`
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	SubjectType string          `json:"subject_type,omitempty"`
	SubjectID   string          `json:"subject_id,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// CreateLibraryRequest is the POST /libraries body.
type CreateLibraryRequest struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

// CreateRootRequest is the POST /libraries/{id}/roots body.
type CreateRootRequest struct {
	Path       string `json:"path"`
	IngestMode string `json:"ingest_mode,omitempty"`
	Enabled    *bool  `json:"enabled,omitempty"`
}

// DesiredItem is content that should exist, whether or not it does (§55).
//
// It anchors to a Work — the semantic entity, which exists whether or not any
// bytes do — and never to an Asset, which is a file that exists by definition.
type DesiredItem struct {
	ID     string `json:"id"`
	Scope  string `json:"scope"`
	WorkID string `json:"work_id"`
	// EditionID is absent at work scope rather than null.
	EditionID        string `json:"edition_id,omitempty"`
	QualityProfileID string `json:"quality_profile_id"`
	Monitor          bool   `json:"monitor"`
	Reason           string `json:"reason,omitempty"`
	// Acquisition is where this want has got to (§64). Absent when the want
	// has no acquisition state — possible only for a row created before the
	// state machine existed.
	Acquisition *AcquisitionState `json:"acquisition,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// AcquisitionState is §64's state: the derived name, plus the four independent
// facts it is a presentation of.
//
// Both of §56's axes are here alongside State on purpose. A client that reads
// only State cannot tell "we have it" from "we have it everywhere", which is
// the distinction the whole model exists to keep.
type AcquisitionState struct {
	State   string `json:"state"`
	Phase   string `json:"phase"`
	Managed bool   `json:"managed"`
	Content string `json:"content"`
	// Placement is proven against a real second peer as of Milestone 4:
	// `converging` is reachable and observed. On a deployment of one peer it
	// is still satisfied the moment Content is, which
	// /desired/{id}/satisfaction reports as `placement.unproven` (ADR-0027).
	Placement string `json:"placement"`
	Detail    string `json:"detail,omitempty"`
}

// WorkDescriptor names content semantically, for wanting something that does
// not exist yet.
type WorkDescriptor struct {
	ContentType string `json:"content_type"`
	Title       string `json:"title"`
	Year        int    `json:"year,omitempty"`
}

// CreateDesiredRequest is the POST /desired body.
//
// Name the work by WorkID or Work, and the profile by QualityProfileID or
// QualityProfile — one of each pair, never both.
type CreateDesiredRequest struct {
	Scope     string          `json:"scope,omitempty"`
	WorkID    string          `json:"work_id,omitempty"`
	EditionID string          `json:"edition_id,omitempty"`
	Work      *WorkDescriptor `json:"work,omitempty"`

	QualityProfileID string `json:"quality_profile_id,omitempty"`
	QualityProfile   string `json:"quality_profile,omitempty"`

	// Monitor is a pointer because absent means "the server's default", which
	// is true. A plain bool could not express "leave it alone".
	Monitor *bool  `json:"monitor,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// UpdateDesiredRequest is the PATCH /desired/{id} body. Every field is a
// pointer: absent means "leave it alone", which is what makes a PATCH a PATCH.
type UpdateDesiredRequest struct {
	QualityProfileID *string `json:"quality_profile_id,omitempty"`
	QualityProfile   *string `json:"quality_profile,omitempty"`
	Monitor          *bool   `json:"monitor,omitempty"`
	Reason           *string `json:"reason,omitempty"`
}

// QualityProfile is the standard a DesiredItem is measured against (§62).
//
// The three sections are three different kinds of statement: Accept is a gate,
// Prefer is a score, Terminal is a stop condition.
type QualityProfile struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Accept      json.RawMessage `json:"accept"`
	Prefer      json.RawMessage `json:"prefer"`
	Terminal    json.RawMessage `json:"terminal"`
	Seeded      bool            `json:"seeded"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}
