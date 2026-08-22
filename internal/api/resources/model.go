package resources

import (
	"database/sql"
	"encoding/json"
	"time"
)

// The wire types.
//
// They are hand-written rather than reused from the persistence layer on
// purpose: the API is a contract with clients and the schema is not, and a
// struct that is both makes every column rename a breaking API change. Every
// timestamp is RFC 3339 in UTC (ADR-0017), which is what time.Time marshals to.

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

// Asset is a file belonging to an Edition.
//
// BlobHash is null for a `linked` asset and that is not an omission: a linked
// asset has no blob at all (ADR-0020), which is what keeps §14's immutability
// absolute.
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

// Blob is byte identity and nothing else (ADR-0005). The bytes themselves are
// served by /blobs/{hash}/content, which is a separate contract (ADR-0013).
type Blob struct {
	Hash        string    `json:"hash"`
	Size        int64     `json:"size"`
	MIME        *string   `json:"mime"`
	Chunked     bool      `json:"chunked"`
	FirstSeenAt time.Time `json:"first_seen_at"`
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

// Peer is a node in the instance.
//
// PublicKey is the peer's Ed25519 identity, rendered as "ed25519:<hex>" — the
// same algorithm-prefixed shape as a blob digest, so the two are not mistaken
// for each other in a terminal. It is null for a peer that has not established
// an identity yet (M4-03).
//
// It is a PUBLIC key and is safe here by construction: the private half lives
// in a 0600 file in the data directory and is never read by the API layer at
// all. An operator enrolling this node at another site needs a value to copy,
// and "read it out of SQLite" is not an answer.
type Peer struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Site      string    `json:"site"`
	Mode      string    `json:"mode"`
	Endpoint  *string   `json:"endpoint"`
	PublicKey *string   `json:"public_key"`
	IsSelf    bool      `json:"is_self"`
	CreatedAt time.Time `json:"created_at"`
	// EnrolledAt is when this peer was admitted to the fabric (M4-04). For the
	// self peer it equals CreatedAt; for anything else it is the moment an
	// operator pinned its key. Revocation is deletion (ADR-0012), so a peer
	// removed and re-enrolled has a NEW id and a new enrolled_at — which is
	// what makes this field the answer to "has membership changed?" rather
	// than a duplicate of created_at.
	EnrolledAt time.Time `json:"enrolled_at"`
	// Health is reachability: unknown, reachable or unreachable (§31, §32,
	// M4-10). It is observed rather than declared — a peer is reachable
	// because it answered something recently, not because it said so — and
	// unreachable means silence past the window rather than a failed request.
	Health string `json:"health"`
	// LastSeenAt is when this peer last answered anything, or null if it never
	// has. It ships beside Health rather than instead of it because
	// "unreachable" alone is a status nobody can act on: it does not say
	// whether to go and reboot something or to wait twenty seconds. Same
	// argument as PlacementVerdict.Missing.
	LastSeenAt *time.Time `json:"last_seen_at"`
	// Snapshot is the peer's materialised catalog snapshot (§52, M4-13), or
	// null when this controller has never issued it one.
	//
	// Explicitly null rather than an omitted key, and explicitly null rather
	// than a zero object. "This peer holds a snapshot of an empty library" and
	// "this peer holds no snapshot" are different answers — in Milestone 7 one
	// means the library is empty and the other means the peer cannot help you
	// — and a client that had to distinguish them by inspecting a row count
	// would eventually get it wrong during an outage, which is the only time
	// it matters.
	Snapshot *PeerSnapshot `json:"snapshot"`
}

// PeerSnapshot is what a peer's catalog snapshot is a fact ABOUT: which
// controller, which version, and how old (§52, §53).
//
// Age ships computed rather than left to the client. A client subtracting
// generated_at from its own clock is a client reporting staleness against a
// clock that is not the controller's — and §53's "conservative rather than
// unavailable" turns on that number being right.
type PeerSnapshot struct {
	// ControllerID is the controller whose catalogue this snapshot is of.
	ControllerID string `json:"controller_id"`
	// Version increases monotonically for this peer.
	Version int64 `json:"version"`
	// GeneratedAt is when the controller read the catalogue.
	GeneratedAt time.Time `json:"generated_at"`
	// Kind is "full" or "incremental" — which path produced this one.
	Kind string `json:"kind"`
	// Rows is how many rows it carries, across every covered table.
	Rows int64 `json:"rows"`
	// ContentDigest fingerprints the contents, so two snapshots can be
	// compared without shipping either.
	ContentDigest string `json:"content_digest"`
	// AgeSeconds is how old the snapshot is, measured against the
	// controller's clock.
	AgeSeconds float64 `json:"age_seconds"`
}

// Replica is one peer's holding of one blob (§8).
//
// It is what the CONTROLLER believes, cached from what the holding peer last
// reported about its own disk (M4-07). ReportedAt is what makes that
// distinction readable rather than implied.
type Replica struct {
	BlobHash     string     `json:"blob_hash"`
	PeerID       string     `json:"peer_id"`
	State        string     `json:"state"`
	BytesPresent int64      `json:"bytes_present"`
	VerifiedAt   *time.Time `json:"verified_at"`
	// ReportedAt is when the holding peer last confirmed this row in an
	// inventory report — null if no peer ever has.
	//
	// Distinct from VerifiedAt, which is when the bytes were last re-hashed. A
	// peer can afford to confirm it still holds a blob far more often than it
	// can afford to read it, so these decay at different rates, and a row
	// nobody has confirmed recently is a fact about the past whatever its
	// state column says.
	ReportedAt *time.Time `json:"reported_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Job is one unit of durable work (§75).
type Job struct {
	ID                 string          `json:"id"`
	Type               string          `json:"type"`
	Payload            json.RawMessage `json:"payload"`
	State              string          `json:"state"`
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

// Token is an API credential. It never carries the secret — after creation the
// secret does not exist anywhere Heyarr can reach.
type Token struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// CreatedToken is the one and only response that contains a secret.
type CreatedToken struct {
	Token Token `json:"token"`
	// Secret is returned exactly once and is not recoverable.
	Secret string `json:"secret"`
}

// ---------------------------------------------------------------------------
// Scanning helpers
// ---------------------------------------------------------------------------

// timeFormat is how every timestamp is stored (ADR-0017).
const timeFormat = time.RFC3339Nano

// parseTime reads a stored timestamp. A value the database holds that will not
// parse is a corrupt row rather than a client error, and the zero time makes
// that visible in the response instead of failing the whole request.
func parseTime(s string) time.Time {
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func parseNullTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseTime(ns.String)
	return &t
}

func nullString(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}

func nullInt(ni sql.NullInt64) *int64 {
	if !ni.Valid {
		return nil
	}
	n := ni.Int64
	return &n
}

// emptyToNil renders an empty string as JSON null, for columns the queue
// coalesces rather than leaves NULL.
func emptyToNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
