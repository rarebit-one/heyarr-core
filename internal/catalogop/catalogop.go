// Package catalogop is the editorial catalog op-log's first increment: a
// signed, content-addressed DELETE tombstone for a work, merged and evaluated
// exactly the way the membership op-set is (ADR-0068, void-which-binds-go ADR-0007).
//
// # Why this exists (ADR-0073, #449, Phase 1)
//
// The move to two-site active-active opens one plane heyarr does not converge:
// the mutable library catalog (works / editions / follow_sources). ADR-0073
// decides that plane converges by an append-only op-log laid as an OVERLAY over
// the scanner-derivable spine — the base (works get-or-created on `work_key`,
// editions the scan re-derives, assets projected from CAS blobs) keeps
// converging indirectly, and only the non-derivable EDITORIAL acts become ops.
// Phase 1 is tombstones: a logical delete at one site must SUPPRESS the other
// site's next scan re-materialising exactly what was removed — the
// "delete-then-rebuild" churn ADR-0071 already refuses inside one node,
// generalised across the pair.
//
// This package is that overlay's core, scoped tightly to WORK deletes. It is
// deliberately the membership idiom, not a new one: a G-set of signed ops keyed
// by content hash, Merge is set union, and Evaluate is a pure function of the
// set — so two peers that have seen different subsets converge once they
// exchange ops, and the answer never depends on arrival order.
//
// # Scope of THIS increment (and what is deliberately out)
//
//   - The entity is the WORK, keyed by its cross-site-stable natural key
//     `(content_type, work_key)` — the `UNIQUE (content_type, work_key)` a
//     rescan already converges on (00002_core.sql). ADR-0073 open question 1
//     names `work_key` as the natural candidate and notes editions have no such
//     key yet; editions/assets therefore wait for that decision.
//   - Two op kinds: `delete` tombstones a work, `restore` lifts a tombstone but
//     only when it causally follows (has cited) the delete it overrides — the
//     observed-remove pattern, so a concurrent restore never resurrects a
//     deleted work and a re-ripped work months later defeats an old tombstone
//     with a fresh op that cites it (ADR-0073 open question 4).
//   - `follow_sources` convergence (Phase 2) and the metadata/`external_id`
//     overlay (Phase 3) are NOT here.
//   - ADR-0119 adds a third kind, `acquire`: the release a site chose for a
//     want on the work, so a sibling can acquire the same bytes itself. It
//     rides the same signed G-set; its view is "newest selection wins" per
//     (target, scope, aspect), suppressed by a tombstone. See Acquire.
//   - WHO may sign a catalog op is ADR-0073 open question 2 — a node/library
//     fact is probably peer-signed under its ADR-0012 identity, which is the
//     model assumed here (`by` is a peer public key). Finer authority and the
//     membership seniority tiebreak are left to the accepted ADR; this
//     increment verifies the signature and treats any validly signed op as
//     authoritative, which is all remove-wins tombstones need.
//
// The wire shape is the cert/op shape — `b64url(payload).b64url(sig)` — so an
// op rides the transport the codebase already has (a token carried beside a
// request, or a content-addressed causal DAG synced like `encrypted_changes`),
// with no new replication machinery.
package catalogop

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// Version prefixes an op payload. It starts at 1: unlike membership, there is
// no earlier cert form to reinterpret, so v1 is the first and only shape.
const Version = 1

// HashPrefix renders an op hash: "sha256:<hex>" over the token's bytes, the id
// a later op cites in `prev`. Identical to the membership op hash scheme.
const HashPrefix = "sha256:"

// MaxPrev bounds how many heads one op may cite, so a hostile op cannot cite
// thousands of hashes to make evaluation quadratic (membership MaxPrev).
const MaxPrev = 64

// unitSep separates the two halves of a target's stable key in a map key. It is
// 0x1f (ASCII Unit Separator), which cannot appear in a content type and is
// vanishingly unlikely in a work key — chosen so `ct+sep+wk` is injective.
const unitSep = "\x1f"

// Kind is what an op does to a work's tombstone state.
type Kind string

const (
	// OpDelete tombstones a work: "the work (ct, wk) is logically removed".
	// Remove wins over any concurrent restore.
	OpDelete Kind = "delete"
	// OpRestore lifts a tombstone, but ONLY when it causally follows the delete
	// it overrides (the delete is in the restore's prev closure). A restore
	// concurrent with a delete does not resurrect the work.
	OpRestore Kind = "restore"
	// OpAcquire publishes the release a site chose for a want on the work
	// (ADR-0119): "for (ct, wk), this scope and aspect, I selected the release
	// with this infohash". A sibling with fan-out enabled acquires the same
	// release with its own download client. Newest selection wins per
	// (target, scope, aspect); a tombstone on the work suppresses it.
	OpAcquire Kind = "acquire"
)

// Acquire is the body of an OpAcquire: what a sibling needs to acquire the
// same bytes, and nothing it must not. It never carries the indexer URL, the
// .torrent, or a credential (ADR-0076): a private release is published with
// Private set and no Trackers, and the sibling resolves it through its own
// indexers by infohash.
type Acquire struct {
	// Scope is where within the work the release lands: ScopeWork, or an
	// edition or item key prefixed "edition:" / "item:".
	Scope string
	// Aspect is AspectPrimary or "subtitle:<lang>".
	Aspect string
	// Profile is the quality profile's NAME at the origin. Profiles are per
	// site (ADR-0038); a sibling without one of that name falls back to the
	// content type's default and records that it did.
	Profile string
	// Infohash is the BitTorrent v1 (40 hex) or v2 (64 hex) infohash, lower-case.
	Infohash string
	// Title is the release title as the indexer named it; display only.
	Title string
	// Size is the release size in bytes as the indexer reported it (0 = unknown).
	Size int64
	// SelectedAt is when the origin selected the release; the newest wins.
	SelectedAt time.Time
	// Site is the origin's site name (peer.site), so a sibling can say where a
	// fan-out want came from.
	Site string
	// Trackers are announce URLs, credential-free by construction (no userinfo,
	// no query, no fragment). Empty when Private.
	Trackers []string
	// Private marks a release from a private indexer: no trackers are
	// published, and the sibling must resolve the infohash with its own account.
	Private bool
}

const (
	// ScopeWork is an Acquire whose release satisfies the work itself.
	ScopeWork = "work"
	// ScopeEditionPrefix prefixes an edition key in Acquire.Scope.
	ScopeEditionPrefix = "edition:"
	// ScopeItemPrefix prefixes an item key in Acquire.Scope.
	ScopeItemPrefix = "item:"
	// AspectPrimary is the primary content of the scope.
	AspectPrimary = "primary"
	// AspectSubtitlePrefix prefixes a language code in Acquire.Aspect.
	AspectSubtitlePrefix = "subtitle:"

	// MaxTrackers bounds the announce list an op may carry.
	MaxTrackers = 16
	// MaxTitle bounds the release title in bytes.
	MaxTitle = 512
	// MaxSite bounds the origin site name in bytes.
	MaxSite = 64
	// MaxProfile bounds the profile name in bytes.
	MaxProfile = 128
	// MaxScope bounds a scope string (prefix + key) in bytes.
	MaxScope = 512
)

// Reason explains why Evaluate rejected a token (it is not part of the state
// and cannot be cited) — the analogue of enrolment's rejection reasons.
type Reason string

const (
	// ReasonMalformed is a token that does not parse or names a bad kind/target.
	ReasonMalformed Reason = "malformed"
	// ReasonBadSignature is a token whose signature does not verify under `by`.
	ReasonBadSignature Reason = "bad_signature"
	// ReasonBadPrev is an op citing a prev that is not a valid op in the set:
	// authority over a delete cannot be judged against a past not seen, so the
	// op is uncitable until its past arrives (membership rule 1).
	ReasonBadPrev Reason = "bad_prev"
)

// Op is one verified catalog statement, as Evaluate consumes it.
type Op struct {
	// Hash is HashPrefix + hex(sha256(Token)) — the id other ops cite in Prev.
	Hash  string
	Token string

	Version  int
	Kind     Kind
	Target   Target
	By       string // the signer's peer public key (ADR-0012), == payload `by`
	Prev     []string
	IssuedAt time.Time
	// Acquire is the body of an OpAcquire; nil for every other kind.
	Acquire *Acquire
}

// Target is a work's cross-site-stable natural key: the pair a rescan converges
// on (`UNIQUE (content_type, work_key)`). It is what an op names instead of the
// per-site UUID `works.id`, which two peers mint differently for one film.
type Target struct {
	ContentType string
	WorkKey     string
}

// Key renders the target as one injective string for use as a map key.
func (t Target) Key() string { return t.ContentType + unitSep + t.WorkKey }

// payload is the v1 signed body. Field order is fixed by this struct so a
// re-marshal produces the same bytes that were signed (membership's opPayload).
type payload struct {
	V           int      `json:"v"`
	Op          Kind     `json:"op"`
	ContentType string   `json:"ct"`
	WorkKey     string   `json:"wk"`
	By          string   `json:"by"`
	Prev        []string `json:"prev"`
	IssuedAt    int64    `json:"iat"`
	// Acq is present on OpAcquire only. omitempty keeps a delete/restore
	// payload byte-identical to the pre-ADR-0119 shape, so every op already
	// in a log keeps its hash.
	Acq *acquirePayload `json:"acq,omitempty"`
}

// acquirePayload is the signed form of Acquire. Field order is fixed by this
// struct for the same canonical re-marshal reason as payload.
type acquirePayload struct {
	Scope      string   `json:"scope"`
	Aspect     string   `json:"aspect"`
	Profile    string   `json:"profile"`
	Infohash   string   `json:"infohash"`
	Title      string   `json:"title"`
	Size       int64    `json:"size"`
	SelectedAt int64    `json:"sel"`
	Site       string   `json:"site"`
	Trackers   []string `json:"trackers"`
	Private    bool     `json:"private"`
}

// Refusals distinct because they name different mistakes.
var (
	// ErrMalformed is a token that is not an op: unparseable, wrong version, an
	// unknown kind, an empty target, or too many prev.
	ErrMalformed = errors.New("catalogop: malformed catalog op")
	// ErrBadSignature is a token whose signature does not verify under its `by`.
	ErrBadSignature = errors.New("catalogop: catalog op signature does not verify")
	// ErrIncomplete is Sign called without the material it needs.
	ErrIncomplete = errors.New("catalogop: incomplete catalog op")
)

var b64 = base64.RawURLEncoding

// Hash renders the id of a token: HashPrefix + hex(sha256(token)).
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return HashPrefix + hex.EncodeToString(sum[:])
}

// Sign mints a v1 catalog op signed by signer over the work (ct, wk): a delete
// tombstones it, a restore lifts a tombstone the op cites in prev. prev are the
// signer's current heads (View.Heads) — the causal evidence a restore has seen
// the delete it overrides; it is sorted and de-duplicated here so the same
// heads always sign to the same bytes.
func Sign(signer ed25519.PrivateKey, kind Kind, ct, wk string, prev []string, issuedAt time.Time) (string, error) {
	if kind != OpDelete && kind != OpRestore {
		return "", fmt.Errorf("%w: unknown kind %q", ErrMalformed, kind)
	}
	return sign(signer, kind, ct, wk, nil, prev, issuedAt)
}

// SignAcquire mints a v1 acquire op (ADR-0119): the signer selected the release
// a describes for the work (ct, wk). It validates a the way Verify will, so a
// sibling never sees an op its own build would refuse.
func SignAcquire(signer ed25519.PrivateKey, ct, wk string, a Acquire, prev []string, issuedAt time.Time) (string, error) {
	ap := a.payload()
	if err := ap.validate(); err != nil {
		return "", err
	}
	return sign(signer, OpAcquire, ct, wk, ap, prev, issuedAt)
}

func sign(signer ed25519.PrivateKey, kind Kind, ct, wk string, acq *acquirePayload, prev []string, issuedAt time.Time) (string, error) {
	if len(signer) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("%w: a signing key is required", ErrIncomplete)
	}
	if ct == "" || wk == "" {
		return "", fmt.Errorf("%w: a content type and work key are required", ErrIncomplete)
	}
	if issuedAt.IsZero() {
		return "", fmt.Errorf("%w: an issued-at is required", ErrIncomplete)
	}
	prev = normalisePrev(prev)
	if len(prev) > MaxPrev {
		return "", fmt.Errorf("%w: %d prev, max %d", ErrMalformed, len(prev), MaxPrev)
	}
	by := identity.FormatPublicKey(signer.Public().(ed25519.PublicKey))
	body, err := json.Marshal(payload{
		V:           Version,
		Op:          kind,
		ContentType: ct,
		WorkKey:     wk,
		By:          by,
		Prev:        prev,
		IssuedAt:    issuedAt.UTC().Unix(),
		Acq:         acq,
	})
	if err != nil {
		return "", fmt.Errorf("catalogop: encoding payload: %w", err)
	}
	sig := ed25519.Sign(signer, body)
	return b64.EncodeToString(body) + "." + b64.EncodeToString(sig), nil
}

func (a Acquire) payload() *acquirePayload {
	trackers := a.Trackers
	if trackers == nil {
		trackers = []string{}
	}
	var sel int64
	if !a.SelectedAt.IsZero() {
		sel = a.SelectedAt.UTC().Unix()
	}
	return &acquirePayload{
		Scope:      a.Scope,
		Aspect:     a.Aspect,
		Profile:    a.Profile,
		Infohash:   a.Infohash,
		Title:      a.Title,
		Size:       a.Size,
		SelectedAt: sel,
		Site:       a.Site,
		Trackers:   trackers,
		Private:    a.Private,
	}
}

func (ap *acquirePayload) acquire() *Acquire {
	return &Acquire{
		Scope:      ap.Scope,
		Aspect:     ap.Aspect,
		Profile:    ap.Profile,
		Infohash:   ap.Infohash,
		Title:      ap.Title,
		Size:       ap.Size,
		SelectedAt: time.Unix(ap.SelectedAt, 0).UTC(),
		Site:       ap.Site,
		Trackers:   append([]string(nil), ap.Trackers...),
		Private:    ap.Private,
	}
}

// validate is the whole ADR-0119 shape rule, applied at Sign and at Verify.
func (ap *acquirePayload) validate() error {
	switch {
	case ap.Scope == ScopeWork:
	case strings.HasPrefix(ap.Scope, ScopeEditionPrefix) && len(ap.Scope) > len(ScopeEditionPrefix):
	case strings.HasPrefix(ap.Scope, ScopeItemPrefix) && len(ap.Scope) > len(ScopeItemPrefix):
	default:
		return fmt.Errorf("%w: acquire scope %q", ErrMalformed, ap.Scope)
	}
	if len(ap.Scope) > MaxScope {
		return fmt.Errorf("%w: acquire scope is %d bytes, max %d", ErrMalformed, len(ap.Scope), MaxScope)
	}
	switch {
	case ap.Aspect == AspectPrimary:
	case strings.HasPrefix(ap.Aspect, AspectSubtitlePrefix) && isLangCode(ap.Aspect[len(AspectSubtitlePrefix):]):
	default:
		return fmt.Errorf("%w: acquire aspect %q", ErrMalformed, ap.Aspect)
	}
	if ap.Profile == "" || len(ap.Profile) > MaxProfile {
		return fmt.Errorf("%w: acquire profile name", ErrMalformed)
	}
	if !isInfohash(ap.Infohash) {
		return fmt.Errorf("%w: acquire infohash %q is not 40 or 64 lower-case hex", ErrMalformed, ap.Infohash)
	}
	if ap.Title == "" || len(ap.Title) > MaxTitle {
		return fmt.Errorf("%w: acquire title", ErrMalformed)
	}
	if ap.Size < 0 {
		return fmt.Errorf("%w: acquire size %d", ErrMalformed, ap.Size)
	}
	if ap.SelectedAt <= 0 {
		return fmt.Errorf("%w: acquire selected-at is required", ErrMalformed)
	}
	if ap.Site == "" || len(ap.Site) > MaxSite {
		return fmt.Errorf("%w: acquire site", ErrMalformed)
	}
	if ap.Trackers == nil {
		return fmt.Errorf("%w: acquire trackers must be a list", ErrMalformed)
	}
	if len(ap.Trackers) > MaxTrackers {
		return fmt.Errorf("%w: %d trackers, max %d", ErrMalformed, len(ap.Trackers), MaxTrackers)
	}
	if ap.Private && len(ap.Trackers) > 0 {
		return fmt.Errorf("%w: a private acquire carries no trackers", ErrMalformed)
	}
	for _, t := range ap.Trackers {
		if err := checkTracker(t); err != nil {
			return err
		}
	}
	return nil
}

// checkTracker admits an announce URL only when it can carry no credential:
// an http(s)/udp/ws(s) URL with a host, no userinfo, no query, no fragment.
// A passkey in the PATH is indistinguishable from a route, which is why a
// private release publishes no trackers at all rather than relying on this.
func checkTracker(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: tracker %q: %w", ErrMalformed, raw, err)
	}
	switch u.Scheme {
	case "http", "https", "udp", "ws", "wss":
	default:
		return fmt.Errorf("%w: tracker %q: scheme %q", ErrMalformed, raw, u.Scheme)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return fmt.Errorf("%w: tracker %q must be host and path only", ErrMalformed, raw)
	}
	return nil
}

func isInfohash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isLangCode admits a two- or three-letter lower-case code (ISO 639-1/-2),
// optionally with a "-REGION" tail of letters, the shape a subtitle aspect uses.
func isLangCode(s string) bool {
	base, tail, hasTail := strings.Cut(s, "-")
	if len(base) < 2 || len(base) > 3 {
		return false
	}
	for i := 0; i < len(base); i++ {
		if base[i] < 'a' || base[i] > 'z' {
			return false
		}
	}
	if !hasTail {
		return true
	}
	if len(tail) < 2 || len(tail) > 8 {
		return false
	}
	for i := 0; i < len(tail); i++ {
		c := tail[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// Verify parses a token, checks its signature under the `by` it carries, and
// returns the Op. It does NOT judge causal validity — Evaluate does, over the
// whole set — so a structurally sound op for a past not yet seen still parses.
func Verify(token string) (Op, error) {
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return Op{}, fmt.Errorf("%w: no separator", ErrMalformed)
	}
	body, err := b64.DecodeString(token[:dot])
	if err != nil {
		return Op{}, fmt.Errorf("%w: payload is not base64url: %w", ErrMalformed, err)
	}
	sig, err := b64.DecodeString(token[dot+1:])
	if err != nil {
		return Op{}, fmt.Errorf("%w: signature is not base64url: %w", ErrMalformed, err)
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Op{}, fmt.Errorf("%w: payload is not JSON: %w", ErrMalformed, err)
	}
	if p.V != Version {
		return Op{}, fmt.Errorf("%w: version %d, this build reads %d", ErrMalformed, p.V, Version)
	}
	switch p.Op {
	case OpDelete, OpRestore:
		if p.Acq != nil {
			return Op{}, fmt.Errorf("%w: a %s op carries an acquire body", ErrMalformed, p.Op)
		}
	case OpAcquire:
		if p.Acq == nil {
			return Op{}, fmt.Errorf("%w: an acquire op without a body", ErrMalformed)
		}
		if err := p.Acq.validate(); err != nil {
			return Op{}, err
		}
	default:
		return Op{}, fmt.Errorf("%w: unknown kind %q", ErrMalformed, p.Op)
	}
	if p.ContentType == "" || p.WorkKey == "" {
		return Op{}, fmt.Errorf("%w: an empty target", ErrMalformed)
	}
	if len(p.Prev) > MaxPrev {
		return Op{}, fmt.Errorf("%w: %d prev, max %d", ErrMalformed, len(p.Prev), MaxPrev)
	}
	pub, err := identity.ParsePublicKey(p.By)
	if err != nil {
		return Op{}, fmt.Errorf("%w: by: %w", ErrMalformed, err)
	}
	if !ed25519.Verify(pub, body, sig) {
		return Op{}, ErrBadSignature
	}
	// Re-marshal must reproduce the signed bytes exactly, or the token carries a
	// payload that is not what its signature covers (a re-ordered or padded
	// duplicate that would hash differently yet verify). Rejecting it keeps the
	// content hash an honest identity.
	canon, err := json.Marshal(p)
	if err != nil {
		return Op{}, fmt.Errorf("catalogop: re-encoding payload: %w", err)
	}
	if string(canon) != string(body) {
		return Op{}, fmt.Errorf("%w: payload is not canonical", ErrMalformed)
	}
	op := Op{
		Hash:     Hash(token),
		Token:    token,
		Version:  p.V,
		Kind:     p.Op,
		Target:   Target{ContentType: p.ContentType, WorkKey: p.WorkKey},
		By:       p.By,
		Prev:     p.Prev,
		IssuedAt: time.Unix(p.IssuedAt, 0).UTC(),
	}
	if p.Acq != nil {
		op.Acquire = p.Acq.acquire()
	}
	return op, nil
}

// Merge is the CRDT join: the union of op token sets, de-duplicated by hash and
// returned in hash order so equal sets are equal slices. It does not verify —
// Evaluate does — so a junk token merges like any other and is rejected there.
// Byte-for-byte the membership Merge.
func Merge(sets ...[]string) []string {
	byHash := make(map[string]string)
	for _, set := range sets {
		for _, tok := range set {
			if tok == "" {
				continue
			}
			byHash[Hash(tok)] = tok
		}
	}
	hashes := make([]string, 0, len(byHash))
	for h := range byHash {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	out := make([]string, len(hashes))
	for i, h := range hashes {
		out[i] = byHash[h]
	}
	return out
}

// View is the evaluated tombstone state: the pure function of the op set.
type View struct {
	// Tombstoned holds every work with an un-overridden effective delete, keyed
	// by Target.Key(). A scan's get-or-create for a key in here must be
	// suppressed; a materialised row for it must be removed.
	Tombstoned map[string]Target
	// Accepted holds every structurally valid op by hash — the state a peer
	// records and replicates.
	Accepted map[string]Op
	// Rejected holds structurally invalid tokens by hash (of the raw token)
	// with the reason; they are not part of the state and cannot be cited.
	Rejected map[string]Reason
	// Heads is the frontier of the valid DAG (ops nothing cites), sorted — what
	// a restore cites as prev when it overrides a delete.
	Heads []string
	// Acquisitions holds, per AcquireKey (target, scope, aspect), the effective
	// acquire op (ADR-0119): the newest selection, ties broken by hash, for a
	// work that is not tombstoned.
	Acquisitions map[string]Op
}

// IsTombstoned reports whether a work is currently tombstoned in the view.
func (v View) IsTombstoned(t Target) bool { _, ok := v.Tombstoned[t.Key()]; return ok }

// AcquireKey renders the key Acquisitions is indexed by.
func AcquireKey(t Target, scope, aspect string) string {
	return t.Key() + unitSep + scope + unitSep + aspect
}

// AcquisitionFor returns the effective acquire op for (target, scope, aspect).
func (v View) AcquisitionFor(t Target, scope, aspect string) (Op, bool) {
	op, ok := v.Acquisitions[AcquireKey(t, scope, aspect)]
	return op, ok
}

// Evaluate turns a bag of op tokens into the current tombstone set. Like the
// membership evaluation it is a state-based CRDT over a grow-only set of signed
// ops: the STATE is the set of structurally valid ops keyed by hash (a G-set),
// MERGE is set union, and the VIEW is a pure function of that state — nothing
// depends on the order tokens arrive in, because every judgement about an op is
// made from the op's own causal past (its prev closure), a property of the set.
//
// Remove wins, causally: a work is tombstoned iff it has an effective delete
// that NO restore causally follows. A restore lifts a tombstone only by citing
// (transitively) the delete it overrides; a restore concurrent with a delete
// leaves the work tombstoned. This is the observed-remove pattern with the
// delete as the tombstone — the same discipline membership uses for a removed
// device that keeps re-adding itself.
func Evaluate(tokens []string) View {
	v := View{
		Tombstoned:   map[string]Target{},
		Accepted:     map[string]Op{},
		Rejected:     map[string]Reason{},
		Acquisitions: map[string]Op{},
	}

	// Pass 1: parse + signature-check. A token that fails is rejected and
	// uncitable (keyed by the hash of the raw token).
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		h := Hash(tok)
		if _, seen := v.Accepted[h]; seen {
			continue
		}
		if _, seen := v.Rejected[h]; seen {
			continue
		}
		op, err := Verify(tok)
		switch {
		case err == nil:
			v.Accepted[h] = op
		case errors.Is(err, ErrBadSignature):
			v.Rejected[h] = ReasonBadSignature
		default:
			v.Rejected[h] = ReasonMalformed
		}
	}

	// Pass 2: structural resolution. An op every prev of which is an accepted op
	// stands; one citing a missing (or rejected) prev is bad_prev and removed
	// from the state — its causal past is not present, so its authority to lift
	// a tombstone cannot be judged (membership rule 1). Iterate to a fixpoint:
	// evicting one op can invalidate another that cited it.
	for {
		evicted := false
		for h, op := range v.Accepted {
			for _, p := range op.Prev {
				if _, ok := v.Accepted[p]; !ok {
					delete(v.Accepted, h)
					v.Rejected[h] = ReasonBadPrev
					evicted = true
					break
				}
			}
		}
		if !evicted {
			break
		}
	}

	// Pass 3: remove-wins per target. Group the surviving ops by target, then a
	// target is tombstoned iff some delete of it is NOT causally covered by a
	// restore (the delete is in no restore's prev closure).
	type group struct {
		target   Target
		deletes  []Op
		restores []Op
	}
	groups := map[string]*group{}
	for _, op := range v.Accepted {
		g := groups[op.Target.Key()]
		if g == nil {
			g = &group{target: op.Target}
			groups[op.Target.Key()] = g
		}
		switch op.Kind {
		case OpDelete:
			g.deletes = append(g.deletes, op)
		case OpRestore:
			g.restores = append(g.restores, op)
		}
	}
	for key, g := range groups {
		tombstoned := false
		for _, d := range g.deletes {
			overridden := false
			for _, r := range g.restores {
				if closureContains(v.Accepted, r, d.Hash) {
					overridden = true
					break
				}
			}
			if !overridden {
				tombstoned = true
				break
			}
		}
		if tombstoned {
			v.Tombstoned[key] = g.target
		}
	}

	// Pass 4: newest selection wins per (target, scope, aspect), for works that
	// are not tombstoned (ADR-0119). Ties on selected-at fall to the hash so
	// the pick is a function of the set.
	for _, op := range v.Accepted {
		if op.Kind != OpAcquire || op.Acquire == nil {
			continue
		}
		if _, dead := v.Tombstoned[op.Target.Key()]; dead {
			continue
		}
		key := AcquireKey(op.Target, op.Acquire.Scope, op.Acquire.Aspect)
		cur, ok := v.Acquisitions[key]
		if !ok || newerAcquire(op, cur) {
			v.Acquisitions[key] = op
		}
	}

	v.Heads = heads(v.Accepted)
	return v
}

// newerAcquire reports whether a should replace b as the effective acquire:
// later selected-at wins; equal selected-at falls to the greater hash.
func newerAcquire(a, b Op) bool {
	if !a.Acquire.SelectedAt.Equal(b.Acquire.SelectedAt) {
		return a.Acquire.SelectedAt.After(b.Acquire.SelectedAt)
	}
	return a.Hash > b.Hash
}

// closureContains reports whether target's hash is in op's prev closure over
// the accepted set (op itself excluded). Content-addressing makes the DAG
// acyclic — a hash depends on its prev, so a cycle is unconstructible — but a
// visited set guards against a malformed set all the same.
func closureContains(accepted map[string]Op, op Op, target string) bool {
	visited := map[string]struct{}{}
	var walk func(o Op) bool
	walk = func(o Op) bool {
		for _, p := range o.Prev {
			if p == target {
				return true
			}
			if _, seen := visited[p]; seen {
				continue
			}
			visited[p] = struct{}{}
			parent, ok := accepted[p]
			if !ok {
				continue
			}
			if walk(parent) {
				return true
			}
		}
		return false
	}
	return walk(op)
}

// heads returns the frontier of the accepted DAG: ops no accepted op cites in
// prev, in sorted hash order so the frontier is a function of the set.
func heads(accepted map[string]Op) []string {
	cited := map[string]struct{}{}
	for _, op := range accepted {
		for _, p := range op.Prev {
			cited[p] = struct{}{}
		}
	}
	var out []string
	for h := range accepted {
		if _, ok := cited[h]; !ok {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// normalisePrev sorts and de-duplicates prev so the same heads always produce
// the same signed bytes (membership normalisePrev). An empty prev stays an
// empty slice so a no-prev op marshals `"prev":[]` rather than `"prev":null`.
func normalisePrev(prev []string) []string {
	if len(prev) == 0 {
		return []string{}
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(prev))
	for _, p := range prev {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
