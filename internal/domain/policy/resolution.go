package policy

// ResolutionClass maps a frame's pixel dimensions to the resolution class a
// §62 profile means when it writes 1080.
//
// # Why this is not simply the height
//
// AttrResolution is documented as "vertical lines: 480, 720, 1080, 2160" —
// a number rather than a label, so a profile never has to know whether an
// indexer spelled it "4K", "2160p" or "UHD". That is right, and taking the
// frame's pixel height as that number is wrong, because the two coincide only
// for 16:9 content.
//
// Cinematic content is not 16:9. The frame is cropped vertically and the width
// is what stays put:
//
//	aspect   1080p frame   height
//	16:9     1920x1080     1080
//	1.85:1   1920x1038     1038
//	2.00:1   1920x960       960
//	2.35:1   1920x816       816
//	2.39:1   1920x800       800
//
// Every one of those is 1080p. Only the first passes a `resolution >= 1080`
// gate when the attribute is the height, so the gate rejects most real
// widescreen film and television — confidently, reporting a `fail` with a
// detail sentence rather than an honest `undetermined`. 2160p breaks the same
// way: a 2.39:1 UHD master is 3840x1600.
//
// So the class comes from the WIDTH, which the crop does not touch, with the
// height as a second opinion for anamorphic storage — 1440x1080 HDV is 1080p
// and its width says otherwise.
//
// # Standard definition falls back to the height, deliberately
//
// Below 720p the width stops discriminating: 720x480 and 720x576 are both 720
// wide and are 480 and 576 line formats respectively. There is no width
// threshold that separates them, so SD keeps the height it always had. This is
// the one range where the old behaviour was already correct.
//
// ok is false when neither dimension is known, which leaves the attribute
// ABSENT and therefore reported as undetermined — "nobody looked" rather than
// a confident zero.
func ResolutionClass(width, height int64) (class int64, ok bool) {
	if width <= 0 && height <= 0 {
		return 0, false
	}
	switch {
	case width >= 3840 || height >= 2160:
		return 2160, true
	case width >= 2560 || height >= 1440:
		return 1440, true
	case width >= 1920 || height >= 1080:
		return 1080, true
	case width >= 1280 || height >= 720:
		return 720, true
	default:
		// Standard definition, where the height is the honest signal. A frame
		// with no height at all but some width lands here only if that width
		// is under 1280, which is already SD; reporting its height of 0 would
		// be a confident lie, so treat it as unknown.
		if height <= 0 {
			return 0, false
		}
		return height, true
	}
}
