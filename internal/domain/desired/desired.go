package desired

import (
	"errors"
	"fmt"
	"strings"
)

// Scope is how much of a Work is wanted.
//
// # Why there is no episode scope, and what that costs
//
// §55's own example is `"content_id": "episode_456"`, and Heyarr cannot
// currently express it. §11 makes the WORK the series, not the episode — an
// edition is a season, and an episode is an Asset, which is a file that
// exists. There is therefore no entity anywhere in the content model for "the
// fifth episode of season two, which I do not have", and a want has nothing to
// point at.
//
// Inventing one here would be a content-model change smuggled into a
// desired-state issue, and it would be the wrong shape besides: knowing which
// episodes SHOULD exist is exactly what a metadata provider is for, and that
// is deferred out of Milestone 3 deliberately. When one lands it can enumerate
// expected episodes, and episode scope becomes an addition rather than a
// retrofit.
//
// What this costs today is stated plainly rather than papered over: with no
// metadata provider, Heyarr does not know how many episodes a season should
// have, so content satisfaction for a series-scoped want is "an acceptable
// asset exists", NOT "every episode exists". A want for a series is not a
// completeness guarantee in Milestone 3, and §56's evaluation must not be read
// as one.
type Scope string

const (
	// ScopeWork wants the whole thing: a film, a series, an album, a book.
	ScopeWork Scope = "work"
	// ScopeEdition wants one edition of it: a season, a particular release, a
	// specific language or format.
	ScopeEdition Scope = "edition"
)

// Scopes lists every scope, in a stable order.
func Scopes() []Scope { return []Scope{ScopeWork, ScopeEdition} }

// ParseScope validates a scope from the wire.
func ParseScope(s string) (Scope, error) {
	for _, v := range Scopes() {
		if string(v) == s {
			return v, nil
		}
	}
	return "", fmt.Errorf("scope must be one of work, edition, not %q", s)
}

// Item is a DesiredItem: this content should exist under these conditions.
type Item struct {
	ID string

	// Scope decides which of the two targets below is meaningful.
	Scope Scope
	// WorkID is always set. It is the semantic anchor, and it is what makes a
	// want expressible before any bytes exist.
	WorkID string
	// EditionID is set only when Scope is ScopeEdition.
	EditionID string

	// QualityProfileID is the standard this want is measured against (§62).
	// Required: "this should exist" with no statement of what would count as
	// existing is not a want, it is a wish, and §56 cannot evaluate it.
	QualityProfileID string

	// Monitor is "keep looking for something better", and is NOT the same as
	// wanting. An unmonitored item that is satisfied is finished, terminal
	// profile or not: the operator said "get me this", not "keep improving
	// this".
	Monitor bool

	// Reason is free text an operator may attach — "for the flight", "Kate
	// asked". It exists because a library accumulates wants and six months
	// later nobody remembers why one is there. It is never interpreted.
	Reason string
}

// maxReason bounds the free-text note. Long enough for a sentence, short
// enough that it is not a place to store a document.
const maxReason = 500

// Target returns the entity this want points at, and which kind it is. It is
// the pair every read path needs and the pair it is easiest to get wrong by
// reading EditionID without checking Scope.
func (i Item) Target() (kind string, id string) {
	if i.Scope == ScopeEdition {
		return "edition", i.EditionID
	}
	return "work", i.WorkID
}

// Validate checks a want, returning the first problem with enough context to
// fix it.
func (i *Item) Validate() error {
	i.WorkID = strings.TrimSpace(i.WorkID)
	i.EditionID = strings.TrimSpace(i.EditionID)
	i.QualityProfileID = strings.TrimSpace(i.QualityProfileID)
	i.Reason = strings.TrimSpace(i.Reason)

	if i.Scope == "" {
		i.Scope = ScopeWork
	}
	if _, err := ParseScope(string(i.Scope)); err != nil {
		return err
	}
	if i.WorkID == "" {
		// Always required, even at edition scope. An edition without its work
		// would make every "what do I want from this series" query a join
		// through the editions table to find out, and would let an edition
		// scope survive its work being deleted.
		return errors.New("a desired item must name the work it wants")
	}
	if i.QualityProfileID == "" {
		return errors.New("a desired item must name a quality profile — " +
			"\"this should exist\" with no statement of what would count as existing " +
			"cannot be evaluated (§56)")
	}

	// The scope and the target must agree. An edition id sitting unused on a
	// work-scoped want is the kind of field that later gets read by something
	// that forgot to check the scope.
	switch i.Scope {
	case ScopeEdition:
		if i.EditionID == "" {
			return errors.New("an edition-scoped desired item must name the edition it wants")
		}
	default:
		if i.EditionID != "" {
			return errors.New("a work-scoped desired item must not name an edition — " +
				"use scope \"edition\" to want one edition of a work")
		}
	}

	if len(i.Reason) > maxReason {
		return fmt.Errorf("the reason is %d characters, past the limit of %d",
			len(i.Reason), maxReason)
	}
	return nil
}

// SameWant reports whether two items express the same want, ignoring identity
// and the fields an operator may change freely.
//
// This is the uniqueness rule (§61: never one version per title). Two wants
// over the same target with DIFFERENT profiles are two different wants — the
// living-room copy and the phone copy — and both must be able to exist. Two
// over the same target with the SAME profile are one want written twice.
func SameWant(a, b Item) bool {
	aKind, aID := a.Target()
	bKind, bID := b.Target()
	return aKind == bKind && aID == bID && a.QualityProfileID == b.QualityProfileID
}
