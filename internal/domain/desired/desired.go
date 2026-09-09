package desired

import (
	"errors"
	"fmt"
	"strings"
)

// Scope is how much of a Work is wanted.
//
// # The three scopes, and the item scope's history
//
// §55's own example is `"content_id": "episode_456"`. For Milestones 3–11
// Heyarr could not express it: §11 makes the WORK the series, not the episode —
// an edition is a season, and an episode is an Asset, which is a file that
// exists. There was therefore no entity anywhere in the content model for "the
// fifth episode of season two, which I do not have", and a want had nothing to
// point at.
//
// Inventing one as a bare desired-state field would have been a content-model
// change smuggled into a desired-state issue, and the wrong shape besides:
// knowing which episodes SHOULD exist is exactly what a metadata provider is
// for, and that was deferred out of Milestone 3 deliberately. The note here
// said the resolution plainly — "when one lands it can enumerate expected
// episodes, and episode scope becomes an addition rather than a retrofit."
//
// M12's feed adapter — a CapabilityMetadata provider — is that metadata
// provider, and ScopeItem is that sanctioned addition. It lands TOGETHER with
// the Item entity (a byte-less row between Edition and Asset, ADR-0056) and
// item-scoped satisfaction, which is what makes it an addition rather than the
// smuggling the note warned against. A want may now point at one Item; and a
// work-scoped want over a series whose items a feed adapter has enumerated
// finally CAN mean "every episode exists" (acquisition.EvaluateCompleteness is
// that fold over the item verdicts) — the completeness guarantee this note said
// was impossible without a metadata provider.
type Scope string

const (
	// ScopeWork wants the whole thing: a film, a series, an album, a book.
	ScopeWork Scope = "work"
	// ScopeEdition wants one edition of it: a season, a particular release, a
	// specific language or format.
	ScopeEdition Scope = "edition"
	// ScopeItem wants one Item of it: a single episode, a podcast entry, a
	// video, an article — the byte-less thing a source emitted (ADR-0056).
	//
	// This is the scope a followed source projects each new item onto: the feed
	// adapter enumerates the items, and one DesiredItem at this scope is created
	// per item so the EXISTING acquisition pipeline archives it. It is distinct
	// from an edition (a season is a grouping of items, not an item) and from a
	// work (the series is the whole thing).
	ScopeItem Scope = "item"
)

// Scopes lists every scope, in a stable order.
func Scopes() []Scope { return []Scope{ScopeWork, ScopeEdition, ScopeItem} }

// ParseScope validates a scope from the wire.
func ParseScope(s string) (Scope, error) {
	for _, v := range Scopes() {
		if string(v) == s {
			return v, nil
		}
	}
	return "", fmt.Errorf("scope must be one of work, edition, item, not %q", s)
}

// Aspect is which facet of a want's target it is about (ADR-0085).
//
// Almost every want is about the target's own content — the video, the audio,
// the document. A subtitle is a companion facet of that same target: not a thing
// a source emitted (an Item, ADR-0056), but a caption for the video an Item
// already is. Modelling a wanted subtitle as an aspect of a want over the
// existing target, rather than a fifth axis on acquisition.State, is ADR-0085's
// core decision — content and placement are two questions about ONE blob, and a
// subtitle is a DIFFERENT asset acquired by a different subsystem at a different
// time, so it is a different WANT, distinguished from the primary want on the
// same target by this aspect.
type Aspect string

const (
	// AspectPrimary is the target's own content. It is the default, and what
	// every want made before ADR-0085 is.
	AspectPrimary Aspect = "primary"
	// AspectSubtitle is a subtitle for the target's video, in the want's
	// Language. It is satisfied by a role='subtitle' asset of that language on
	// the target's edition, fetched direct from a subtitle provider — never
	// searched on an indexer (ADR-0085 routes it RouteDirect regardless of the
	// work's content type).
	AspectSubtitle Aspect = "subtitle"
)

// Aspects lists every aspect, in a stable order.
func Aspects() []Aspect { return []Aspect{AspectPrimary, AspectSubtitle} }

// ParseAspect validates an aspect from the wire. An empty string is the primary
// aspect, so a caller that knows nothing of aspects keeps working unchanged.
func ParseAspect(s string) (Aspect, error) {
	if s == "" {
		return AspectPrimary, nil
	}
	for _, v := range Aspects() {
		if string(v) == s {
			return v, nil
		}
	}
	return "", fmt.Errorf("aspect must be one of primary, subtitle, not %q", s)
}

// normAspect treats the empty aspect as primary, so a want stored before
// ADR-0085 (no aspect column) and one written as "primary" are one want.
func normAspect(a Aspect) Aspect {
	if a == "" {
		return AspectPrimary
	}
	return a
}

// Item is a DesiredItem: this content should exist under these conditions.
type Item struct {
	ID string

	// Scope decides which of the targets below is meaningful.
	Scope Scope
	// WorkID is always set. It is the semantic anchor, and it is what makes a
	// want expressible before any bytes exist. An Item belongs to a Work too,
	// so an item-scoped want still names it.
	WorkID string
	// EditionID is set only when Scope is ScopeEdition.
	EditionID string
	// ItemID is set only when Scope is ScopeItem — the byte-less Item this want
	// points at (ADR-0056). The item's own edition grouping lives on the Item
	// row, not here, so an item-scoped want never also carries an EditionID.
	ItemID string

	// Aspect is which facet of the target this want is about (ADR-0085).
	// AspectPrimary (the default) is the content itself; AspectSubtitle is a
	// caption for the target's video, in Language. It is part of the want's
	// identity, so the primary want and the subtitle want over one target with
	// one profile are two distinct wants.
	Aspect Aspect
	// Language is the subtitle language as an ISO-639-1 code (e.g. "en"), set
	// only when Aspect is AspectSubtitle. It too is part of identity: the English
	// subtitle and the German subtitle of one episode are two wants.
	Language string

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
	switch i.Scope {
	case ScopeItem:
		return "item", i.ItemID
	case ScopeEdition:
		return "edition", i.EditionID
	default:
		return "work", i.WorkID
	}
}

// Validate checks a want, returning the first problem with enough context to
// fix it.
func (i *Item) Validate() error {
	i.WorkID = strings.TrimSpace(i.WorkID)
	i.EditionID = strings.TrimSpace(i.EditionID)
	i.ItemID = strings.TrimSpace(i.ItemID)
	i.QualityProfileID = strings.TrimSpace(i.QualityProfileID)
	i.Reason = strings.TrimSpace(i.Reason)
	i.Language = strings.ToLower(strings.TrimSpace(i.Language))

	if i.Scope == "" {
		i.Scope = ScopeWork
	}
	if _, err := ParseScope(string(i.Scope)); err != nil {
		return err
	}
	i.Aspect = normAspect(i.Aspect)
	if _, err := ParseAspect(string(i.Aspect)); err != nil {
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

	// The scope and the target must agree. A target id sitting unused on a want
	// of a different scope is the kind of field that later gets read by
	// something that forgot to check the scope. Each arm requires its own id and
	// refuses the other two scopes' ids.
	switch i.Scope {
	case ScopeItem:
		if i.ItemID == "" {
			return errors.New("an item-scoped desired item must name the item it wants")
		}
		if i.EditionID != "" {
			return errors.New("an item-scoped desired item must not name an edition — " +
				"the item's grouping lives on the item row, not on the want")
		}
	case ScopeEdition:
		if i.EditionID == "" {
			return errors.New("an edition-scoped desired item must name the edition it wants")
		}
		if i.ItemID != "" {
			return errors.New("an edition-scoped desired item must not name an item — " +
				"use scope \"item\" to want one item of a work")
		}
	default:
		if i.EditionID != "" {
			return errors.New("a work-scoped desired item must not name an edition — " +
				"use scope \"edition\" to want one edition of a work")
		}
		if i.ItemID != "" {
			return errors.New("a work-scoped desired item must not name an item — " +
				"use scope \"item\" to want one item of a work")
		}
	}

	// The aspect and its language must agree, and a subtitle must point at
	// something with concrete video bytes to caption (ADR-0085).
	switch i.Aspect {
	case AspectSubtitle:
		if i.Language == "" {
			return errors.New("a subtitle want must name a language — " +
				"the English subtitle and the German subtitle of one target are two wants")
		}
		if i.Scope == ScopeWork {
			// A work is a series or a film, not a concrete releasable video. A
			// subtitle captions one edition or one item; a "subtitles for the
			// whole series" intent is the completeness fold over per-item subtitle
			// wants, not a single work-scoped one.
			return errors.New("a subtitle want must be scoped to an item or an edition, " +
				"not the whole work — a work has no single video to caption")
		}
	default:
		if i.Language != "" {
			return errors.New("only a subtitle want carries a language; " +
				"a primary want must not name one")
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
	return aKind == bKind && aID == bID &&
		a.QualityProfileID == b.QualityProfileID &&
		normAspect(a.Aspect) == normAspect(b.Aspect) &&
		a.Language == b.Language
}
