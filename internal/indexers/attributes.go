package indexers

import (
	"strconv"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// Attribute extraction: what a release SAYS about itself, and nothing else.
//
// ---------------------------------------------------------------------------
// AN ATTRIBUTE THAT CANNOT BE DETERMINED IS LEFT OUT. IT IS NEVER DEFAULTED.
// ---------------------------------------------------------------------------
//
// §63 reports `undetermined` only for an attribute it never received, and that
// is the difference between "this release is 720p and you wanted 1080p" and
// "nobody could tell what resolution this is". The first is a fact about the
// release; the second is a fact about the provider, and an operator seeing it
// knows to look somewhere else entirely.
//
// A defaulted attribute destroys that distinction silently: it produces a
// confident wrong answer with no reason attached, and the reason is the thing
// §63 exists to give.
//
// # Nothing here reads the title
//
// It would be easy. Every release in the captured corpus has its content in
// its name, and a dozen regular expressions would populate resolution, codec
// and source for most of them, most of the time.
//
// The precedent not to repeat is M2's HDR detection: a substring match on the
// ffprobe profile string, recorded as a known weakness. A release title is
// worse evidence than ffprobe output, not better — it is a filename written
// by a stranger, with no schema and no obligation to be true. Attributes
// derived from one would be indistinguishable, downstream, from attributes an
// indexer actually asserted, and §63's explanation would be reporting
// confidence in a guess.
//
// So the rule is: an attribute comes from a torznab:attr or a document
// element, or it does not exist.

// torznabAttribute maps a Torznab attribute name onto policy's closed
// vocabulary.
//
// Only names with a fixture behind them are here, and the fixtures are not
// all equal. ADR-0026 is explicit that a branch of the parser with no fixture
// behind it is a branch that has never seen reality and must be either
// fixtured or deleted.
//
// BE PRECISE ABOUT WHICH OF THESE HAS SEEN A REAL SERVER:
//
//	size_bytes   REAL. Both captured servers emit <size> on every item.
//	resolution   SYNTHESISED, and so are video, audio and language.
//
// The reason is not laziness. The only tracker safe to capture from into a
// public repository indexes Linux distributions, and a Linux ISO asserts no
// resolution, no codec and no audio layout. So the real corpus cannot contain
// a positive case for any of them, and the synthesised fixture that does is
// labelled as invented where the next person meets it.
//
// What this means in practice, and it should be said plainly rather than
// discovered: against the two indexers actually measured, this provider
// determines a release's SIZE and nothing else. Every quality rule in a §62
// profile evaluates to `undetermined` on their candidates. That is the honest
// state of discovery in this milestone, and closing it is a metadata
// provider's job rather than a better regular expression over titles.
var torznabAttribute = map[string]policy.Attribute{
	"size":       policy.AttrSizeBytes,
	"resolution": policy.AttrResolution,
	"video":      policy.AttrVideoCodec,
	"audio":      policy.AttrAudioCodec,
	"language":   policy.AttrLanguage,
}

// attributesOf is everything the release actually asserted.
func attributesOf(i item) acquisition.Attributes {
	attrs := acquisition.Attributes{}

	if n, ok := sizeOf(i); ok {
		attrs[policy.AttrSizeBytes] = policy.Num(n)
	}

	for name, mapped := range torznabAttribute {
		if mapped == policy.AttrSizeBytes {
			// Handled by sizeOf, which reads three sources in order of trust.
			continue
		}
		raw, ok := i.attr(name)
		if !ok {
			continue
		}
		value, ok := valueFor(mapped, raw)
		if !ok {
			// The attribute was present and could not be understood — a
			// resolution of "hd", a codec that is a category id. LEFT OUT
			// rather than coerced: an unreadable value is not evidence, and
			// pretending otherwise is the defaulting this file refuses.
			continue
		}
		attrs[mapped] = value
	}

	// An empty map and a nil map mean the same thing to §63, and returning
	// nil makes "the provider determined nothing" visible in a debugger
	// rather than looking like an initialised-but-unpopulated result.
	if len(attrs) == 0 {
		return nil
	}
	return attrs
}

// valueFor converts a raw attribute value into policy's typed form, declining
// anything it cannot read.
func valueFor(a policy.Attribute, raw string) (policy.Value, bool) {
	kind, known := policy.KindOf(a)
	if !known {
		return policy.Value{}, false
	}
	raw = strings.TrimSpace(raw)
	switch kind {
	case policy.KindInt:
		// Torznab spells a resolution both ways — "1080" and "1080p" — and
		// the trailing p is a spelling of the same number rather than a
		// different claim. Nothing else is stripped: "hd" is not a number and
		// must not become one.
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.ToLower(raw), "p"), 10, 64)
		if err != nil {
			return policy.Value{}, false
		}
		return policy.Num(n), true
	case policy.KindText:
		return policy.Text(raw), true
	case policy.KindFlag:
		switch strings.ToLower(raw) {
		case "true", "yes", "1":
			return policy.Flag(true), true
		case "false", "no", "0":
			return policy.Flag(false), true
		default:
			return policy.Value{}, false
		}
	default:
		return policy.Value{}, false
	}
}

// sizeOf reads a release's size from the three places a server may put it.
//
// In order of trust: the <size> element, which both captured servers emit and
// which is the protocol's own field; a torznab:attr saying the same thing;
// and the enclosure's length attribute, which is really the torrent file's
// own claim. They are read in that order and the first that parses wins —
// rather than being cross-checked — because a disagreement between them is
// not something this client can resolve, and the protocol's own field is the
// one the server meant.
func sizeOf(i item) (int64, bool) {
	for _, raw := range []string{i.Size, attrOrEmpty(i, "size"), i.Enclosure.Length} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			continue
		}
		return n, true
	}
	return 0, false
}

func attrOrEmpty(i item, name string) string {
	v, ok := i.attr(name)
	if !ok {
		return ""
	}
	return v
}
