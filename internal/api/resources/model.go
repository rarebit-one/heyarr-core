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

// Peer is a node in the instance. The public key is deliberately not exposed
// yet: it is reserved for Milestone 4 (ADR-0012) and publishing a field before
// it means anything invites clients to depend on its absence.
type Peer struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Site      string    `json:"site"`
	Mode      string    `json:"mode"`
	Endpoint  *string   `json:"endpoint"`
	IsSelf    bool      `json:"is_self"`
	CreatedAt time.Time `json:"created_at"`
}

// Replica is one peer's holding of one blob (§8).
type Replica struct {
	BlobHash     string     `json:"blob_hash"`
	PeerID       string     `json:"peer_id"`
	State        string     `json:"state"`
	BytesPresent int64      `json:"bytes_present"`
	VerifiedAt   *time.Time `json:"verified_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
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
