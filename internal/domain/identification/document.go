package identification

import "strings"

// docExts are the extensions a web document lands as. A followed article is
// archived as a self-contained single-file `.html` (ADR-0063); the others are
// the shapes a hand-curated document shelf holds.
var docExts = newSet(".html", ".htm", ".xhtml", ".mhtml", ".mht")

// isDocumentPath reports whether a document rule should look at this path.
// Companion extensions are accepted for the same reason the other content
// types accept them: roleView has already rewritten the path to point at the
// sibling primary file.
func isDocumentPath(p Path) bool { return docExts[p.Ext] || isAuxExt(p.Ext) }

// documentRules identify a §12 Document — a captured web article or an imported
// HTML page.
//
// # Why these are minimal, and why they still have to exist
//
// The path that MATTERS for documents never reaches these rules. A followed
// article arrives at ingest with its Work already known: the feed named the
// publication and the article, the item-scoped want carries that identity, and
// ingest's WorkOverride path attaches the captured blob to that Work without
// running filename identification over the `.html` (ADR-0063 §3). The captured
// file's name is a transfer digest, not a title, so parsing it would be worse
// than useless — which is exactly why the override exists.
//
// They exist for the OTHER path: a `document` library scanned from disk. A
// content type NO rule declares matches nothing, falls back to every rule in
// registration order, and the movie rules are first — the silent-corruption
// shape #227 documents. Registering document rules that claim the document
// extensions keeps a scanned `.html` a document Work rather than a phantom
// movie. They are deliberately shallow: title, and a publisher when a directory
// offers one, which is all a bare page on a shelf honestly carries.
func documentRules() []Rule {
	return []Rule{
		{Name: "document/publisher-title", ContentType: Document, Match: matchDocumentPublisherTitle},
		{Name: "document/title-only", ContentType: Document, Match: matchDocumentTitleOnly},
	}
}

// matchDocumentPublisherTitle is "Publisher/Article Title.html", the natural
// shelf shape: the directory names the publication, the file names the article.
func matchDocumentPublisherTitle(p Path) (Candidate, bool) {
	if !isDocumentPath(p) || len(p.Dirs) == 0 {
		return Candidate{}, false
	}
	publisher := parseName(p.Dir())
	title := parseName(p.Stem)
	if publisher.Empty() || title.Empty() {
		return Candidate{}, false
	}
	return documentCandidate(publisher, title, p), true
}

// matchDocumentTitleOnly is the last resort: a bare titled page with a document
// extension and no directory to help.
func matchDocumentTitleOnly(p Path) (Candidate, bool) {
	if !docExts[p.Ext] {
		return Candidate{}, false
	}
	title := parseName(p.Stem)
	if title.Empty() {
		return Candidate{}, false
	}
	return documentCandidate(nameParts{}, title, p), true
}

func documentCandidate(publisher, title nameParts, p Path) Candidate {
	titleKey := normKey(title.Title)
	if titleKey == "" {
		return Candidate{}
	}
	pubKey := normKey(publisher.Title)

	work := map[string]any{}
	if pubKey != "" {
		work["publisher"] = displayTitle(publisher.Title)
	}

	format := strings.TrimPrefix(p.Ext, ".")
	editionType := format
	if !docExts[p.Ext] {
		// A companion file inherits the work but names no edition of its own.
		format, editionType = "default", ""
	}
	label := ""
	if editionType != "" {
		label = strings.ToUpper(editionType)
	}

	return Candidate{
		ContentType:       Document,
		WorkKey:           workKey(Document, pubKey, titleKey, yearKey(title.Year)),
		Title:             displayTitle(title.Title),
		SortTitle:         titleKey,
		Year:              title.Year,
		WorkAttributes:    work,
		EditionAttributes: map[string]any{},
		EditionKey:        format,
		EditionLabel:      label,
		EditionType:       editionType,
	}
}
