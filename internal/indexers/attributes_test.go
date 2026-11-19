package indexers

import (
	"slices"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// titleAttributes reads a release name's quality into policy's vocabulary, every
// value marked Derived (ADR-0091).
func TestTitleAttributesDerived(t *testing.T) {
	attrs := titleAttributes("Slow.Horses.S01E01.1080p.WEB-DL.DDP5.1.x265-GRP")

	res, ok := attrs[policy.AttrResolution]
	if !ok || res.Num != 1080 {
		t.Errorf("resolution = %+v, want 1080", res)
	}
	src, ok := attrs[policy.AttrSource]
	if !ok || src.String() != "web-dl" {
		t.Errorf("source = %+v, want web-dl", src)
	}
	vc, ok := attrs[policy.AttrVideoCodec]
	if !ok || vc.String() != "hevc" {
		t.Errorf("video_codec = %+v, want hevc (x265 folds to hevc)", vc)
	}
	for a, v := range attrs {
		if !v.Derived {
			t.Errorf("attribute %s is not marked Derived: %+v", a, v)
		}
	}
	// A name asserts no size, so size is never derived — it is an assertion or
	// it is absent.
	if _, ok := attrs[policy.AttrSizeBytes]; ok {
		t.Error("size must not be derived from a title")
	}
}

func TestTitleAttributesHDRFlag(t *testing.T) {
	attrs := titleAttributes("Some.Film.2160p.WEB-DL.DV.HDR.H265-GRP")
	hdr, ok := attrs[policy.AttrHDR]
	if !ok || !hdr.Flag || !hdr.Derived {
		t.Errorf("hdr = %+v, want derived flag true (DV or HDR both count)", hdr)
	}
}

func TestTitleAttributesUnrecognisedIsNil(t *testing.T) {
	if got := titleAttributes("Some Perfectly Ordinary Title"); got != nil {
		t.Errorf("titleAttributes on a name with no quality tokens = %+v, want nil", got)
	}
}

// The fill-only-absent merge: an asserted value is never overwritten by a
// derived one, and a slot the indexer left empty is filled.
func TestFillAbsentStructuredWins(t *testing.T) {
	dst := acquisition.Attributes{policy.AttrResolution: policy.Num(720)} // asserted
	fillAbsent(dst, acquisition.Attributes{
		policy.AttrResolution: policy.Num(1080).AsDerived(),
		policy.AttrSource:     policy.Text("web-dl").AsDerived(),
	})

	if got := dst[policy.AttrResolution]; got.Num != 720 || got.Derived {
		t.Errorf("asserted resolution was overwritten: %+v, want asserted 720", got)
	}
	if got, ok := dst[policy.AttrSource]; !ok || !got.Derived {
		t.Errorf("absent source was not filled from the title: %+v", got)
	}
}

// prowlarrAttributes is size-only with parsing off, and size (asserted) plus
// title-derived quality with it on.
func TestProwlarrAttributesParseTitles(t *testing.T) {
	size := int64(900_000_000)
	r := prowlarrRelease{Title: "Slow.Horses.S01E01.1080p.WEB-DL.x265-GRP", Size: &size}

	off := prowlarrAttributes(r, false, "")
	if _, ok := off[policy.AttrResolution]; ok {
		t.Error("resolution present with parse_titles off — nothing should read the title")
	}
	if got := off[policy.AttrSizeBytes]; got.Num != size || got.Derived {
		t.Errorf("size = %+v, want asserted %d", got, size)
	}

	on := prowlarrAttributes(r, true, "")
	if got := on[policy.AttrResolution]; got.Num != 1080 || !got.Derived {
		t.Errorf("resolution with parse on = %+v, want derived 1080", got)
	}
	if got := on[policy.AttrSizeBytes]; got.Num != size || got.Derived {
		t.Errorf("size must stay asserted even with parse on: %+v", got)
	}
}

// Prowlarr's seed count is asserted, so it is read whatever parse_titles says
// (#888), and a release that carries none (usenet) has no seeders attribute.
func TestProwlarrAttributesCarrySeeders(t *testing.T) {
	seeders := int64(42)
	got := prowlarrAttributes(prowlarrRelease{Title: "x", Seeders: &seeders}, false, "")
	if v, ok := got[policy.AttrSeeders]; !ok || v.Num != 42 || v.Derived {
		t.Errorf("seeders = %+v (present %v), want asserted 42", v, ok)
	}
	if _, ok := prowlarrAttributes(prowlarrRelease{Title: "x"}, false, "")[policy.AttrSeeders]; ok {
		t.Error("a release with no seed count gained a seeders attribute")
	}
}

func TestResolutionLines(t *testing.T) {
	cases := map[string]struct {
		want int64
		ok   bool
	}{
		"2160p": {2160, true},
		"1080p": {1080, true},
		"1080i": {1080, true},
		"720p":  {720, true},
		"":      {0, false},
		"hd":    {0, false},
	}
	for in, want := range cases {
		got, ok := resolutionLines(in)
		if got != want.want || ok != want.ok {
			t.Errorf("resolutionLines(%q) = (%d, %v), want (%d, %v)", in, got, ok, want.want, want.ok)
		}
	}
}

func TestVideoCodecClass(t *testing.T) {
	cases := map[string]string{
		"x265": "hevc", "h265": "hevc", "hevc": "hevc",
		"x264": "h264", "h264": "h264", "avc": "h264",
		"av1": "av1", "vp9": "vp9", "xvid": "xvid", "": "",
	}
	for in, want := range cases {
		if got := videoCodecClass(in); got != want {
			t.Errorf("videoCodecClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// A name that spells out several audio languages reads as the SET of them,
// marked derived (#888, ADR-0091), and one that names none carries no language.
func TestTitleAttributesLanguageSet(t *testing.T) {
	attrs := titleAttributes("Some.Film.2023.1080p.WEB-DL.Org.Hindi.DDP.2.0.Eng-GRP")
	lang, ok := attrs[policy.AttrLanguage]
	if !ok || !slices.Equal(lang.Texts, []string{"hi", "en"}) || !lang.Derived {
		t.Errorf("language = %+v (present %v), want derived set [hi en]", lang, ok)
	}
	if _, ok := titleAttributes("Slow.Horses.S01E01.1080p.WEB-DL.x265-GRP")[policy.AttrLanguage]; ok {
		t.Error("a name naming no language gained one")
	}
}

// A torznab language attribute is a set too: one name, or several separated by
// commas or slashes, and asserted rather than derived.
func TestTorznabLanguageIsASet(t *testing.T) {
	cases := map[string][]string{
		"en":             {"en"},
		"Hindi, English": {"hindi", "english"},
		"ita/eng":        {"ita", "eng"},
		" en ,, fr ":     {"en", "fr"},
	}
	for raw, want := range cases {
		got, ok := valueFor(policy.AttrLanguage, raw)
		if !ok || !slices.Equal(got.Texts, want) || got.Derived {
			t.Errorf("valueFor(language, %q) = %+v (ok %v), want asserted set %v", raw, got, ok, want)
		}
	}
	if _, ok := valueFor(policy.AttrLanguage, " , / "); ok {
		t.Error("a language attribute naming nothing was read as a set")
	}
}

// The case #888 reported, end to end through the title parser: with the
// household language default the config documents (prefer en +50, penalise a
// list of dubs that includes hi by 100), a release whose name is Hindi AND
// English earns the bonus and the penalty together, and so ranks below a plain
// English release — and below an untagged one — on the seeded `everyday`
// profile it would otherwise have tied with.
func TestAHindiAndEnglishReleaseRanksBelowEnglish(t *testing.T) {
	var everyday policy.Profile
	for _, p := range policy.Defaults() {
		if p.Name == "everyday" {
			everyday = p
		}
	}
	if everyday.Name == "" {
		t.Fatal("the seeded everyday profile is missing")
	}
	profile := everyday.WithLanguageDefault(policy.LanguageDefault{
		Prefer: "en", Weight: 50, Penalty: 100,
		Foreign: []string{"it", "es", "fr", "de", "ja", "ko", "zh", "pt", "ru", "pl", "tr", "hi", "nl", "ar"},
	})

	size := int64(4_000_000_000)
	cand := func(id, title string) acquisition.ReleaseCandidate {
		return acquisition.ReleaseCandidate{
			ID: id, Title: title, Provider: "test",
			Attributes: prowlarrAttributes(prowlarrRelease{Title: title, Size: &size}, true, ""),
		}
	}
	// Listed first, as the reported release was.
	ranked := acquisition.EvaluateAll([]acquisition.ReleaseCandidate{
		cand("a-hindi-english", "Some.Film.2023.1080p.60fps.WEB-DL.Org.Hindi.DDP.2.0.Eng-GRP"),
		cand("b-untagged", "Some.Film.2023.1080p.WEB-DL.DDP5.1.x264-GRP"),
		cand("c-english", "Some.Film.2023.1080p.WEB-DL.English.DDP5.1.x264-GRP"),
	}, profile)

	var order []string
	for _, r := range ranked {
		if !r.Evaluation.Accepted {
			t.Errorf("%s was rejected; language must never gate: %+v", r.Candidate.ID, r.Evaluation)
		}
		order = append(order, r.Candidate.ID)
	}
	if want := []string{"c-english", "b-untagged", "a-hindi-english"}; !slices.Equal(order, want) {
		t.Errorf("ranking = %v, want %v", order, want)
	}
}
