package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/rarebit-one/heyarr-core/internal/api/resources"
)

// The ADR-0075 browse surface, reached by an agent.
//
// These are the shelf reads a consumer client draws — the catalog walked
// rather than searched, the artists and authors it groups by, the resume rail,
// and what a subscription has archived. Each is a shell over the SAME exported
// resources intent the HTTP browse handlers call (BrowseWorks, ListGrouped,
// ContinueRail, FollowedSourceItems), so the two doors cannot drift — the
// discipline search_content is built on. Only the truncatable envelope, which
// keeps a listing inside a model's context, is this door's own.

// browseLibrary walks the catalog with the browse embeds (ADR-0075). It is the
// counterpart to search_content: search resolves a title, this walks a shelf.
func (s *Server) browseLibrary(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ContentType         string `json:"content_type"`
		LibraryID           string `json:"library_id"`
		Query               string `json:"q"`
		Artist              string `json:"artist"`
		Author              string `json:"author"`
		Year                *int64 `json:"year"`
		YearFrom            *int64 `json:"year_from"`
		YearTo              *int64 `json:"year_to"`
		Sort                string `json:"sort"`
		IncludeArtwork      *bool  `json:"include_artwork"`
		IncludePrimaryAsset *bool  `json:"include_primary_asset"`
		Limit               int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	recent := false
	switch args.Sort {
	case "", "title":
	case "recent":
		recent = true
	default:
		return nil, invalidParams("sort must be title or recent, not %q", args.Sort)
	}
	// The embeds default ON: an agent browsing a shelf wants the poster and the
	// playable file, and asking for them is the common case rather than the
	// exception a person would think to request.
	includeArtwork := args.IncludeArtwork == nil || *args.IncludeArtwork
	includePrimary := args.IncludePrimaryAsset == nil || *args.IncludePrimaryAsset

	limit := clampLimit(args.Limit)
	res, err := s.resources.BrowseWorks(ctx, resources.BrowseWorksRequest{
		ContentType:    args.ContentType,
		LibraryID:      args.LibraryID,
		Query:          args.Query,
		Artist:         args.Artist,
		Author:         args.Author,
		Year:           args.Year,
		YearFrom:       args.YearFrom,
		YearTo:         args.YearTo,
		Recent:         recent,
		Limit:          limit,
		IncludeArtwork: includeArtwork,
		IncludePrimary: includePrimary,
	})
	if err != nil {
		return nil, classify(err)
	}
	out := struct {
		truncatable
		Works []resources.WorkCard `json:"works"`
	}{Works: res.Cards}
	out.Count = len(res.Cards)
	// The intent fetched one past the limit and set a cursor when more remain;
	// an agent reading this listing as exhaustive would otherwise miscount a
	// library, so truncation is stated rather than inferred.
	out.Truncated = res.NextCursor != ""
	return out, nil
}

// listGroupedTool is the shared body of list_artists and list_authors: a
// grouping over one attribute a scan wrote, paged and counted.
func (s *Server) listGroupedTool(ctx context.Context, raw json.RawMessage, attr, contentType, collection string) (any, error) {
	var args struct {
		Query string `json:"q"`
		Limit int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	items, next, err := s.resources.ListGrouped(ctx, attr, contentType, collection,
		resources.ListGroupedRequest{Query: args.Query, Limit: clampLimit(args.Limit)})
	if err != nil {
		return nil, classify(err)
	}
	out := struct {
		truncatable
		Groups []resources.GroupSummary `json:"groups"`
	}{Groups: items}
	out.Count = len(items)
	out.Truncated = next != ""
	return out, nil
}

// listArtists groups music works by their artist (ADR-0075).
func (s *Server) listArtists(ctx context.Context, raw json.RawMessage) (any, error) {
	return s.listGroupedTool(ctx, raw, "artist", "music", "artists")
}

// listAuthors groups book works by their author (ADR-0075).
func (s *Server) listAuthors(ctx context.Context, raw json.RawMessage) (any, error) {
	return s.listGroupedTool(ctx, raw, "author", "book", "authors")
}

// continueRail lists the newest positioned, not-finished session per work — the
// "pick up where it stopped" rail (ADR-0075). These are resume points the node
// holds server-side (ADR-0024); the encrypted personal history stays in the
// Personal MCP (§72), which this server holds no key for.
func (s *Server) continueRail(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		DeviceID string `json:"device_id"`
		Limit    int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	limit := clampLimit(args.Limit)
	// One past the limit, so truncation is a fact about the result rather than a
	// guess from a full page.
	entries, err := s.resources.ContinueRail(ctx, resources.ContinueRailRequest{
		DeviceID: args.DeviceID, Limit: int64(limit) + 1,
	})
	if err != nil {
		return nil, classify(err)
	}
	truncated := false
	if len(entries) > limit {
		entries = entries[:limit]
		truncated = true
	}
	out := struct {
		truncatable
		Entries []resources.ContinueEntry `json:"entries"`
	}{Entries: entries}
	out.Count = len(entries)
	out.Truncated = truncated
	return out, nil
}

// followedSourceItems lists what one subscription has archived and merely knows
// about (#430), complementing list_followed's per-source summary. It is the
// per-item detail behind a single followed source.
func (s *Server) followedSourceItems(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		SourceID string `json:"source_id"`
		Limit    int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.SourceID == "" {
		return nil, invalidParams("source_id is required — the source to detail, from list_followed")
	}
	detail, err := s.resources.FollowedSourceDetail(ctx, args.SourceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, invalidParams("there is no followed source with that id")
		}
		return nil, classifyFollow(err)
	}
	limit := clampLimit(args.Limit)
	items, next, err := s.resources.FollowedSourceItems(ctx, args.SourceID, "", limit)
	if err != nil {
		return nil, classifyFollow(err)
	}
	out := struct {
		truncatable
		Source resources.FollowedSourceView `json:"source"`
		Items  []resources.FollowedItemView `json:"items"`
	}{Source: detail, Items: items}
	out.Count = len(items)
	out.Truncated = next != ""
	return out, nil
}

// getProviderStatus reports the configured indexers and download clients, what
// each can do and whether it works — the read behind "why is nothing being
// acquired" (§59, ADR-0025/0026). A node with none configured is a supported
// deployment and says so with an empty list rather than an error. No credential
// is ever reported.
func (s *Server) getProviderStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	out, err := s.resources.ProvidersStatus(ctx)
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}
