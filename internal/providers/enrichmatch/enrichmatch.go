// Package enrichmatch is the shared match-strength vocabulary the enrich
// adapters use to compute an EnrichResult.Confidence (ADR-0087, ADR-0088).
//
// It lives here, not inside one adapter, so a confidence of 0.75 means the SAME
// thing whether MusicBrainz or Open Library produced it — the worker gates
// identity correction on one threshold (ADR-0088 §3), and a threshold is only
// meaningful if every adapter scores on the same scale. The scale is a
// normalised token-set similarity: how many of the words in the shorter side
// also appear in the longer, after case-folding and stripping punctuation. It is
// deliberately order- and duplication-insensitive, because a query title carries
// its words in filename order with the author appended, and the authority
// returns them in canonical order — the words overlap even when the strings do
// not align.
package enrichmatch

import (
	"strings"
	"unicode"
)

// TokenSetRatio is the fraction of the smaller token set that the two strings
// share, 0..1. Both strings are normalised (lower-cased, punctuation dropped,
// split on whitespace) into sets of words; the ratio is |intersection| /
// min(|a|,|b|). Two empty (or unnormalisable) strings score 0 — nothing to be
// confident about.
//
// The MIN rather than the union is deliberate: a cleaned query title ("the
// almanack of naval ravikant") is a subset of the authority's title+author ("the
// almanack of naval ravikant eric jorgenson"), and scoring against the union
// would punish the match for the author words the query never had. min() asks
// "is the query fully contained in the answer", which is the question a
// confident correction turns on.
func TokenSetRatio(a, b string) float64 {
	sa := tokenSet(a)
	sb := tokenSet(b)
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	shared := 0
	for tok := range sa {
		if sb[tok] {
			shared++
		}
	}
	denom := len(sa)
	if len(sb) < denom {
		denom = len(sb)
	}
	return float64(shared) / float64(denom)
}

// tokenSet normalises a string into a set of comparison tokens: lower-cased
// words with punctuation folded to spaces, single characters dropped (a stray
// "a"/"&" carries no match signal and only inflates the denominator).
func tokenSet(s string) map[string]bool {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	out := make(map[string]bool, len(fields))
	for _, f := range fields {
		if len([]rune(f)) < 2 {
			continue
		}
		out[f] = true
	}
	return out
}
