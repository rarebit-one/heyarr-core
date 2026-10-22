package policy

import (
	"fmt"
	"sort"
	"strings"
)

// Kind is the type of an attribute's values.
//
// It exists so that a rule can be validated at WRITE time rather than at
// evaluation time. "minimum_resolution: remux" is a mistake whose natural home
// is the terminal of whoever typed it, not a rejection reason attached to
// every candidate for the next six months.
type Kind string

const (
	// KindInt is a whole number. Comparisons are numeric.
	KindInt Kind = "int"
	// KindText is a name from an open vocabulary — a codec, a source, a
	// language. Comparisons are by equality over a normalised form, never by
	// ordering: there is no answer to whether "hevc" is greater than "av1".
	KindText Kind = "text"
	// KindFlag is a boolean.
	KindFlag Kind = "flag"
)

// Attribute is something a profile rule can assert about a release.
//
// The set is CLOSED, and deliberately so. An open set — "any key the operator
// types" — cannot be validated at write time, which is the entire reason the
// three-section distinction is enforceable at all. Adding an attribute is a
// code change, which is correct: something has to know how to extract it from
// a release (M3-09), and an attribute nothing populates is a rule that
// silently never matches.
type Attribute string

const (
	// AttrResolution is the resolution CLASS in vertical lines: 480, 720,
	// 1080, 2160. A number rather than a label, because "4K" and "2160p" and
	// "UHD" are three spellings of one number and a profile should not have to
	// know which one an indexer used.
	//
	// The class, and NOT the frame's pixel height. They coincide only at 16:9,
	// and a 2.35:1 1080p master is 1920x816 — taking the height rejected it as
	// sub-1080 (#231). See ResolutionClass for how the two relate and why
	// standard definition is the one range that keeps its height.
	AttrResolution Attribute = "resolution"
	// AttrSource is where the release came from: remux, bluray, web-dl,
	// webrip, hdtv, dvd, cam. §62's example uses it as a terminal condition.
	//
	// It is KindText and NOT ordered, even though there is an obvious quality
	// ordering among those names. An ordering here would be a second scoring
	// system living outside `prefer`, invisible to §63's reasons — which is
	// exactly the opaque scoring §61 rejects. An operator who wants
	// "bluray beats webrip" says so with two preferences and can then see both
	// in the explanation.
	AttrSource Attribute = "source"
	// AttrVideoCodec is h264, hevc, av1, vp9.
	AttrVideoCodec Attribute = "video_codec"
	// AttrAudioCodec is aac, ac3, eac3, dts, truehd, flac, opus.
	AttrAudioCodec Attribute = "audio_codec"
	// AttrAudioChannels is a channel count: 2, 6, 8.
	AttrAudioChannels Attribute = "audio_channels"
	// AttrHDR is whether the release claims high dynamic range.
	//
	// Note what this is NOT: M2's HDR detection is a substring match on the
	// ffprobe stream profile and is recorded as a known weakness. A release
	// title is worse evidence than ffprobe output, not better. M3-09 must
	// leave this ABSENT when it cannot tell, so that §63 can report "could not
	// determine" rather than confidently reporting false.
	AttrHDR Attribute = "hdr"
	// AttrSizeBytes is the release size. Useful as a gate — "nothing over
	// 40 GB" — and as a penalty.
	AttrSizeBytes Attribute = "size_bytes"
	// AttrLanguage is an audio language tag.
	AttrLanguage Attribute = "language"
	// AttrStore is the store a copy came from: gog, steam, epic (ADR-0118).
	// Populated from the edition's attributes, which a store fetch writes once
	// it has verified the whole package — never from a scanned filename. An
	// indexer release never carries it, so a profile that gates on it refuses
	// an indexer's repack as undetermined rather than guessing it is owned.
	AttrStore Attribute = "store"
	// AttrDRMFree is whether the copy runs without its store's client. Left
	// ABSENT when the store does not say — a Steam depot may or may not — so
	// §63 reports "could not determine", never a confident false.
	AttrDRMFree Attribute = "drm_free"
	// AttrPlatform is the operating system a game copy installs on (windows,
	// linux, mac), or the console a ROM is for (snes, ps2, …; ADR-0121). Text,
	// not ordered: no platform is better than another in general.
	AttrPlatform Attribute = "platform"
	// AttrRegion is the regions a ROM dump is for, as its No-Intro/Redump name
	// tags them: "usa", "europe", "world", "japan" (ADR-0121 §5). A SET, because
	// one dump is often several regions ("(USA, Europe)"), so a rule holds when
	// ANY of them matches. Read from the release name, never asserted, and only
	// ever preferred: it ranks releases, it does not gate them.
	AttrRegion Attribute = "region"
	// AttrIndexerPrivacy is the privacy class of the indexer a release came
	// from, as the indexer manager reports it: "public", "private" or
	// "semi_private". Provenance, not quality: a two-site fan-out (ADR-0119)
	// reads it to decide whether a sibling may join the swarm by infohash or
	// must resolve the release through its own credentialed indexer. Absent
	// when the indexer does not say.
	AttrIndexerPrivacy Attribute = "indexer_privacy"
)

// Indexer privacy classes, the normalised values AttrIndexerPrivacy carries.
const (
	IndexerPublic      = "public"
	IndexerPrivate     = "private"
	IndexerSemiPrivate = "semi_private"
)

// attributeKinds is the vocabulary. A rule naming anything not in here is
// refused at write time.
var attributeKinds = map[Attribute]Kind{
	AttrResolution:     KindInt,
	AttrSource:         KindText,
	AttrVideoCodec:     KindText,
	AttrAudioCodec:     KindText,
	AttrAudioChannels:  KindInt,
	AttrHDR:            KindFlag,
	AttrSizeBytes:      KindInt,
	AttrLanguage:       KindText,
	AttrStore:          KindText,
	AttrDRMFree:        KindFlag,
	AttrPlatform:       KindText,
	AttrRegion:         KindText,
	AttrIndexerPrivacy: KindText,
}

// KindOf reports an attribute's kind, and whether it is known at all.
func KindOf(a Attribute) (Kind, bool) {
	k, ok := attributeKinds[a]
	return k, ok
}

// Attributes lists every known attribute, in a stable order.
//
// Stable because it appears in error messages and in the API, and an error
// message whose wording depends on Go's map iteration is one nobody can grep a
// log for.
func Attributes() []Attribute {
	out := make([]Attribute, 0, len(attributeKinds))
	for a := range attributeKinds {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func attributeNames() string {
	names := Attributes()
	strs := make([]string, len(names))
	for i, n := range names {
		strs[i] = string(n)
	}
	return strings.Join(strs, ", ")
}

// Op is how a rule compares an attribute to its operand.
type Op string

const (
	// OpEq is equality. Legal for every kind.
	OpEq Op = "eq"
	// OpNeq is inequality. Legal for every kind.
	OpNeq Op = "neq"
	// OpGTE is "at least". Numbers only.
	OpGTE Op = "gte"
	// OpLTE is "at most". Numbers only.
	OpLTE Op = "lte"
	// OpIn is membership in a set. Text only — "source in [remux, bluray]".
	OpIn Op = "in"
	// OpNotIn is exclusion from a set. Text only.
	OpNotIn Op = "nin"
)

// opsByKind is which comparisons make sense for which kind.
//
// gte/lte are absent for text on purpose: see AttrSource. `in` is absent for
// numbers because a set membership over integers is a gate an operator almost
// never means — "resolution in [1080, 2160]" excludes 1440 silently, where
// "resolution gte 1080" says what they meant.
var opsByKind = map[Kind][]Op{
	KindInt:  {OpEq, OpNeq, OpGTE, OpLTE},
	KindText: {OpEq, OpNeq, OpIn, OpNotIn},
	KindFlag: {OpEq, OpNeq},
}

func opAllowed(k Kind, op Op) bool {
	for _, allowed := range opsByKind[k] {
		if allowed == op {
			return true
		}
	}
	return false
}

func opNames(k Kind) string {
	ops := opsByKind[k]
	strs := make([]string, len(ops))
	for i, o := range ops {
		strs[i] = string(o)
	}
	return strings.Join(strs, ", ")
}

// normaliseText folds a text operand so that two operators spelling one
// capability differently converge.
//
// The device capability lists do the same thing for the same reason: a profile
// that matches "HEVC" and not "hevc" is a profile that works for whoever wrote
// it and silently rejects everything for the next person.
func normaliseText(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func kindError(a Attribute, k Kind, got string) error {
	return fmt.Errorf("%s is a %s attribute and takes a %s value, not %s", a, k, k, got)
}
