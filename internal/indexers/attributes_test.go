package indexers

import (
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

	off := prowlarrAttributes(r, false)
	if _, ok := off[policy.AttrResolution]; ok {
		t.Error("resolution present with parse_titles off — nothing should read the title")
	}
	if got := off[policy.AttrSizeBytes]; got.Num != size || got.Derived {
		t.Errorf("size = %+v, want asserted %d", got, size)
	}

	on := prowlarrAttributes(r, true)
	if got := on[policy.AttrResolution]; got.Num != 1080 || !got.Derived {
		t.Errorf("resolution with parse on = %+v, want derived 1080", got)
	}
	if got := on[policy.AttrSizeBytes]; got.Num != size || got.Derived {
		t.Errorf("size must stay asserted even with parse on: %+v", got)
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
