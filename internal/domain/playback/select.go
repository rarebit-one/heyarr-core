package playback

import (
	"cmp"
	"fmt"
	"slices"
)

// Choosing among a work's copies (§68, #885).
//
// A film can hold several files — a 2160p HDR remux and a 1080p SDR encode,
// each kept by its own want and profile. Choose and Negotiate plan ONE asset;
// this decides which asset to plan, from what the client says it can show.
// It is pure for the same reason they are: the interesting part is a ranking
// over a handful of facts, and every cell of it should be a table row.
//
// # The rule
//
// A copy FITS when its height is at or under the client's max height and its
// bitrate at or under the client's max bitrate (an unstated limit is no
// limit; an unknown bitrate is not held against a copy). Among the fits:
//
//  1. one that plays without re-encoding video beats one that does — a
//     re-encode costs a core per stream and some picture;
//  2. when the client does not declare HDR, an SDR copy beats an HDR one —
//     nothing here tone-maps (#886), so HDR on an SDR screen looks washed out;
//  3. then the tallest picture;
//  4. then, at equal height, an HDR copy when the client declares HDR, the
//     higher bitrate, and the asset id — so the answer is stable across
//     requests.
//
// When nothing fits, the smallest copy over the limit is chosen and scaled
// down as any too-tall source is. Copies nothing has probed rank last: their
// height is unknown, so they can neither fit nor be the smallest, and are
// chosen only when no copy has been probed at all.

// Copy is one file holding the picture of the thing being played, as
// selection sees it.
type Copy struct {
	AssetID string
	// Known is false when nothing has probed the bytes. Height, HDR,
	// VideoCodec and BitrateBPS are then all unknown.
	Known      bool
	Height     int
	HDR        bool
	VideoCodec string
	// BitrateBPS is zero when the container declares none.
	BitrateBPS int64
}

// Fit is what the client can show: the declaration selection ranks against.
// A zero maximum is "no limit stated", as everywhere in this package.
type Fit struct {
	MaxHeight     int64
	MaxBitrateBPS int64
	SupportsHDR   bool
	// VideoCodecs is what the client decodes; empty means not stated, and no
	// copy is then held to need a re-encode.
	VideoCodecs []string
}

// Selection codes. Stable, like the planner's reason codes: a client shows the
// detail and may branch on the code.
const (
	// SelectedBestFit: the best copy within the client's limits.
	SelectedBestFit = "best_fit"
	// SelectedSmallestOverLimit: nothing fits; the smallest copy over the
	// limit is chosen and will be scaled down.
	SelectedSmallestOverLimit = "smallest_over_limit"
	// SelectedNotProbed: no copy has been probed, so there was nothing to
	// rank by; the lowest asset id is chosen.
	SelectedNotProbed = "not_probed"

	// LostNeedsVideoReencode: it fits, but the client does not decode its
	// video and the chosen copy plays without re-encoding.
	LostNeedsVideoReencode = "needs_video_reencode"
	// LostHDRNotDeclared: it fits, but it is HDR, the client does not
	// declare HDR, and an SDR copy was available.
	LostHDRNotDeclared = "hdr_not_declared"
	// LostSmaller: it fits, and the chosen copy is taller.
	LostSmaller = "smaller"
	// LostEqual: it fits at the chosen copy's height and lost the tie.
	LostEqual = "equal_lost_tie"
	// LostLarger: nothing fits, and it is further over the limit than the
	// chosen copy.
	LostLarger = "larger"
	// LostNotProbed: nothing has probed it, so its height is unknown.
	LostNotProbed = "not_probed"
)

// Candidate is one copy and why it was chosen or passed over.
type Candidate struct {
	Copy
	Reason Reason
}

// Selection is the answer: the copy to plan, and every other copy with the
// reason it lost. "Why did my phone get the 1080p" is this type's question.
type Selection struct {
	Chosen Candidate
	Passed []Candidate
}

// tiers, in preference order.
const (
	tierFits = iota
	tierOverLimit
	tierUnknown
)

// ranked is a copy with the facts the ordering keys on.
type ranked struct {
	Copy
	tier        int
	reencode    bool // the client does not decode the video
	hdrMismatch bool // HDR, and the client does not declare HDR
	tooTall     bool
	tooFast     bool
}

// Select chooses among copies of one thing (§68, #885). ok is false only when
// there are no copies at all.
func Select(copies []Copy, fit Fit) (Selection, bool) {
	if len(copies) == 0 {
		return Selection{}, false
	}
	all := make([]ranked, 0, len(copies))
	for _, c := range copies {
		all = append(all, rank(c, fit))
	}
	slices.SortFunc(all, func(a, b ranked) int { return compareRanked(a, b, fit) })

	chosen := all[0]
	out := Selection{Chosen: Candidate{Copy: chosen.Copy, Reason: chosenReason(chosen, all[1:], fit)}}
	for _, r := range all[1:] {
		out.Passed = append(out.Passed, Candidate{Copy: r.Copy, Reason: passedReason(r, chosen, fit)})
	}
	return out, true
}

func rank(c Copy, fit Fit) ranked {
	r := ranked{Copy: c}
	if !c.Known || c.Height <= 0 {
		r.tier = tierUnknown
		return r
	}
	r.reencode = c.VideoCodec != "" && len(fit.VideoCodecs) > 0 && !contains(fit.VideoCodecs, c.VideoCodec)
	r.hdrMismatch = c.HDR && !fit.SupportsHDR
	r.tooTall = fit.MaxHeight > 0 && int64(c.Height) > fit.MaxHeight
	r.tooFast = fit.MaxBitrateBPS > 0 && c.BitrateBPS > fit.MaxBitrateBPS
	if r.tooTall || r.tooFast {
		r.tier = tierOverLimit
	}
	return r
}

// compareRanked is the whole ordering, so the rule above is read in one place.
func compareRanked(a, b ranked, fit Fit) int {
	if c := cmp.Compare(a.tier, b.tier); c != 0 {
		return c
	}
	switch a.tier {
	case tierFits:
		if c := compareBool(a.reencode, b.reencode); c != 0 {
			return c
		}
		if c := compareBool(a.hdrMismatch, b.hdrMismatch); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Height, a.Height); c != 0 {
			return c
		}
		if fit.SupportsHDR {
			if c := compareBool(!a.HDR, !b.HDR); c != 0 {
				return c
			}
		}
		if c := cmp.Compare(b.BitrateBPS, a.BitrateBPS); c != 0 {
			return c
		}
	case tierOverLimit:
		// The least over: shortest, then SDR when HDR is not declared, then
		// the lowest bitrate.
		if c := cmp.Compare(a.Height, b.Height); c != 0 {
			return c
		}
		if c := compareBool(a.hdrMismatch, b.hdrMismatch); c != 0 {
			return c
		}
		if c := cmp.Compare(a.BitrateBPS, b.BitrateBPS); c != 0 {
			return c
		}
	}
	return cmp.Compare(a.AssetID, b.AssetID)
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	default:
		return 1
	}
}

func chosenReason(c ranked, rest []ranked, fit Fit) Reason {
	switch c.tier {
	case tierFits:
		detail := fmt.Sprintf("the tallest copy (%dp) within %s", c.Height, limits(fit))
		for _, r := range rest {
			if r.tier == tierFits && r.hdrMismatch && !c.hdrMismatch {
				detail += "; SDR preferred because the client does not declare HDR"
				break
			}
		}
		for _, r := range rest {
			if r.tier == tierFits && r.reencode && !c.reencode {
				detail += "; it plays without re-encoding video"
				break
			}
		}
		return Reason{Code: SelectedBestFit, Detail: detail}
	case tierOverLimit:
		return Reason{
			Code: SelectedSmallestOverLimit,
			Detail: fmt.Sprintf("no copy is within %s; the smallest (%dp) is chosen and scaled down",
				limits(fit), c.Height),
		}
	default:
		return Reason{
			Code:   SelectedNotProbed,
			Detail: "no copy has been probed, so there was nothing to rank by; a probe would let this choose",
		}
	}
}

func passedReason(r, chosen ranked, fit Fit) Reason {
	switch {
	case r.tier == tierUnknown:
		return Reason{Code: LostNotProbed, Detail: "nothing has probed this copy, so its height is unknown"}
	case r.tier == tierOverLimit && chosen.tier == tierOverLimit:
		return Reason{Code: LostLarger, Detail: fmt.Sprintf("%dp is further over the limit than the copy chosen", r.Height)}
	case r.tooTall:
		return Reason{
			Code:   ReasonResolutionTooHigh,
			Detail: fmt.Sprintf("%dp is past the client's max height %d", r.Height, fit.MaxHeight),
		}
	case r.tooFast:
		return Reason{
			Code:   ReasonBitrateTooHigh,
			Detail: fmt.Sprintf("%d bit/s is past the client's %d", r.BitrateBPS, fit.MaxBitrateBPS),
		}
	case r.reencode && !chosen.reencode:
		return Reason{
			Code:   LostNeedsVideoReencode,
			Detail: fmt.Sprintf("the client does not declare %s video, and another copy plays without re-encoding", r.VideoCodec),
		}
	case r.hdrMismatch && !chosen.hdrMismatch:
		return Reason{Code: LostHDRNotDeclared, Detail: "HDR, and the client does not declare HDR; an SDR copy fits"}
	case r.Height < chosen.Height:
		return Reason{Code: LostSmaller, Detail: fmt.Sprintf("%dp fits, and a taller copy does too", r.Height)}
	default:
		return Reason{Code: LostEqual, Detail: fmt.Sprintf("%dp, as tall as the copy chosen, which won on HDR, bitrate or id", r.Height)}
	}
}

// limits renders the client's ceilings for a sentence.
func limits(fit Fit) string {
	switch {
	case fit.MaxHeight > 0 && fit.MaxBitrateBPS > 0:
		return fmt.Sprintf("the client's max height %d and %d bit/s", fit.MaxHeight, fit.MaxBitrateBPS)
	case fit.MaxHeight > 0:
		return fmt.Sprintf("the client's max height %d", fit.MaxHeight)
	case fit.MaxBitrateBPS > 0:
		return fmt.Sprintf("the client's %d bit/s", fit.MaxBitrateBPS)
	default:
		return "no stated limit"
	}
}
