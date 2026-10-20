package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/desired"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// AcquireFacts is what an acquire op (ADR-0119) says about a want, read in the
// shape catalogop.Acquire wants, minus the release identity the grab supplies.
type AcquireFacts struct {
	ContentType string
	WorkKey     string
	WorkTitle   string
	Year        int
	// Scope and Aspect are already in the op's spelling (catalogop.ScopeWork,
	// "edition:<key>", "item:<key>"; catalogop.AspectPrimary, "subtitle:<lang>").
	Scope  string
	Aspect string
	// Profile is the quality profile's NAME: ids are per site, names travel.
	Profile string
	// CandidateID and CandidateTitle are the selected release; Size is the
	// indexer's reported size (0 when unknown).
	CandidateID    string
	CandidateTitle string
	Size           int64
	// SelectedAt is when the search that chose this release ran: the
	// persisted selection time, stable across a retried grab, so a re-run
	// republishes the SAME selection rather than a newer-looking one.
	SelectedAt time.Time
	// Private is whether the release came from a private (or semi-private)
	// indexer, per the indexer manager's privacy class on the candidate. An
	// indexer that reports no class is treated as public, which at worst sends
	// a sibling to a swarm it cannot join, never a credential anywhere.
	Private bool
}

// AcquireFacts reads the facts an acquire op carries for a want: its work's
// natural key and description, its scope and aspect spelled the op's way, the
// profile's name, and the selected release's id, title and size. It fails with
// ErrNoSelection when nothing is selected, since there is then nothing to tell
// a sibling.
func (c *Catalog) AcquireFacts(ctx context.Context, desiredItemID string) (AcquireFacts, error) {
	var (
		f             AcquireFacts
		year          sql.NullInt64
		scope, aspect string
		language      string
		editionKey    sql.NullString
		itemKey       sql.NullString
		attrs         sql.NullString
		searchedAt    string
	)
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT w.content_type, w.work_key, w.title, w.year,
		       d.scope, d.aspect, d.language, e.edition_key, i.item_key, q.name,
		       rc.candidate_id, rc.title, rc.attributes, rc.searched_at
		FROM desired_items d
		JOIN works w ON w.id = d.work_id
		JOIN quality_profiles q ON q.id = d.quality_profile_id
		LEFT JOIN editions e ON e.id = d.edition_id
		LEFT JOIN items i ON i.id = d.item_id
		JOIN release_candidates rc ON rc.desired_item_id = d.id AND rc.selected = 1
		WHERE d.id = ?`, desiredItemID).
		Scan(&f.ContentType, &f.WorkKey, &f.WorkTitle, &year,
			&scope, &aspect, &language, &editionKey, &itemKey, &f.Profile,
			&f.CandidateID, &f.CandidateTitle, &attrs, &searchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AcquireFacts{}, fmt.Errorf("%w: %s", ErrNoSelection, desiredItemID)
	}
	if err != nil {
		return AcquireFacts{}, fmt.Errorf("catalog: reading acquire facts for %s: %w", desiredItemID, err)
	}
	f.Year = int(year.Int64)

	switch desired.Scope(scope) {
	case desired.ScopeEdition:
		if !editionKey.Valid {
			return AcquireFacts{}, fmt.Errorf("catalog: want %s is edition-scoped but names no edition", desiredItemID)
		}
		f.Scope = catalogop.ScopeEditionPrefix + editionKey.String
	case desired.ScopeItem:
		if !itemKey.Valid {
			return AcquireFacts{}, fmt.Errorf("catalog: want %s is item-scoped but names no item", desiredItemID)
		}
		f.Scope = catalogop.ScopeItemPrefix + itemKey.String
	default:
		f.Scope = catalogop.ScopeWork
	}
	if desired.Aspect(aspect) == desired.AspectSubtitle {
		f.Aspect = catalogop.AspectSubtitlePrefix + strings.ToLower(language)
	} else {
		f.Aspect = catalogop.AspectPrimary
	}
	f.SelectedAt = parseStamp(searchedAt)
	if attrs.Valid {
		f.Size, f.Private = candidateFacts(attrs.String)
	}
	return f, nil
}

// candidateFacts pulls the indexer's size_bytes (0 when absent) and privacy
// class out of a candidate's stored attributes.
func candidateFacts(raw string) (size int64, private bool) {
	var a acquisition.Attributes
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return 0, false
	}
	if v, ok := a[policy.AttrSizeBytes]; ok && v.Kind == policy.KindInt && v.Num >= 0 {
		size = v.Num
	}
	if v, ok := a[policy.AttrIndexerPrivacy]; ok && v.Kind == policy.KindText {
		private = v.Text == policy.IndexerPrivate || v.Text == policy.IndexerSemiPrivate
	}
	return size, private
}
