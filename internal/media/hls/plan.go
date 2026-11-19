// Package hls serves the on-the-fly repackage (ADR-0069) as a seekable HLS
// presentation (ADR-0126): a VOD playlist whose segment boundaries are known
// before a byte is produced, and segments made on demand by one ffmpeg that is
// retargeted when a player seeks.
//
// # Why the boundaries are computed, not discovered
//
// A player maps a seek to a segment by adding up the playlist's durations, and
// then fetches that segment by number. So segment N must mean the same span of
// the film whichever ffmpeg produced it: the one that started at the beginning
// and has been running for an hour, or the one started a second ago because
// the viewer jumped there. ffmpeg's own HLS muxer cannot promise that — it cuts
// at the first keyframe after a running total, which depends on where it
// started — so this package computes the boundaries up front and cuts a
// continuous fragmented MP4 at them itself:
//
//   - A copied picture can only be cut at the source's keyframes, so the
//     boundaries ARE source keyframes, read once from the file (Index).
//   - A re-encoded picture has the keyframes the encoder is told to make, so
//     the boundaries are every SegmentSeconds and the encoder is forced to put
//     a keyframe at each one.
package hls

import (
	"fmt"
	"math"
	"strings"
)

// SegmentSeconds is the target segment length. Six seconds is what Apple's
// authoring guidance asks for and what every HLS player in the house is tuned
// to buffer; shorter would cost a request and a fragment header per two
// seconds of film, longer would make a seek land further from where it aimed.
const SegmentSeconds = 6.0

// Segment is one media segment: where it starts on the segment timeline and
// how long it plays.
type Segment struct {
	Start    float64
	Duration float64
}

// PlanKeyframes cuts a copied picture at its own keyframes.
//
// keyframes are the presentation times of the video's keyframes, ascending;
// end is where the film ends on the same clock. A segment runs from one
// boundary keyframe to the first keyframe at least target later, so it is
// never shorter than the target except at the end, and never longer than
// the target plus one GOP. A source with no keyframes listed gets no plan: it
// cannot be cut, only streamed.
func PlanKeyframes(keyframes []float64, end, target float64) []Segment {
	if len(keyframes) == 0 || target <= 0 {
		return nil
	}
	var bounds []float64
	for _, k := range keyframes {
		if k >= end {
			break
		}
		if len(bounds) == 0 || k >= bounds[len(bounds)-1]+target {
			bounds = append(bounds, k)
		}
	}
	if len(bounds) == 0 {
		return nil
	}
	// A last segment shorter than a second is folded into the one before it:
	// a player that fetches a few frames of credits as a whole segment gains
	// nothing, and some refuse a segment shorter than their own frame step.
	if n := len(bounds); n > 1 && end-bounds[n-1] < 1 {
		bounds = bounds[:n-1]
	}
	return segmentsBetween(bounds, end)
}

// PlanUniform cuts a re-encoded picture every target seconds, from zero to
// duration.
func PlanUniform(duration, target float64) []Segment {
	if duration <= 0 || target <= 0 {
		return nil
	}
	n := int(math.Ceil(duration/target - 1e-9))
	if n > 1 && duration-float64(n-1)*target < 1 {
		n--
	}
	bounds := make([]float64, n)
	for i := range bounds {
		bounds[i] = float64(i) * target
	}
	return segmentsBetween(bounds, duration)
}

func segmentsBetween(bounds []float64, end float64) []Segment {
	out := make([]Segment, len(bounds))
	for i, b := range bounds {
		next := end
		if i+1 < len(bounds) {
			next = bounds[i+1]
		}
		out[i] = Segment{Start: b, Duration: next - b}
	}
	return out
}

// Locate is the segment a fragment starting at t belongs to: the last whose
// start is at or before t, within tolerance. -1 is before the first.
//
// The tolerance absorbs a timestamp that went through a 90 kHz clock on the
// way out of ffmpeg, and a forced keyframe that lands on the first frame at
// or after its boundary rather than exactly on it. It is far below a frame,
// so it cannot move a fragment across a boundary that a real keyframe sits on.
func Locate(segments []Segment, t float64) int {
	lo, hi := 0, len(segments)-1
	found := -1
	for lo <= hi {
		mid := (lo + hi) / 2
		if segments[mid].Start-locateTolerance <= t {
			found, lo = mid, mid+1
		} else {
			hi = mid - 1
		}
	}
	return found
}

const locateTolerance = 0.005

// Playlist renders the media playlist: VOD, the whole film, every segment
// listed, one init segment. Segment URIs are relative — "init.mp4" and
// "<n>.m4s" — so the playlist works wherever it is served from, including
// behind a relay that re-roots the path.
func Playlist(segments []Segment) string {
	target := 1
	for _, s := range segments {
		// RFC 8216 requires EXT-X-TARGETDURATION to be at least every
		// EXTINF rounded to the nearest integer.
		if d := int(math.Round(s.Duration)); d > target {
			target = d
		}
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", target)
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	b.WriteString("#EXT-X-MAP:URI=\"init.mp4\"\n")
	for i, s := range segments {
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n%d.m4s\n", s.Duration, i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}
