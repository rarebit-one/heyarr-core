package playback

// A person's preferred playback quality (#887).
//
// What a screen can show is a fact about the device; how much of it the person
// wants is a fact about the person, and it follows them from phone to
// television. It is personal state, so the node never reads it: the client
// decrypts it, lays this device's override over it, and sends the result with
// the plan request (Invariant 6, ADR-0123's addendum). This file is what the
// node does with what it is sent: turn it into the limits selection and the
// planner already read. It is pure for the same reason Select is.
//
// # The rule
//
//   - fit (the default, and what an absent preference means): the screen's
//     height is the limit, as before.
//   - best: the screen's height is no limit. The tallest copy is chosen and
//     handed over without scaling; a screen downscales a picture better than a
//     re-encode does.
//   - data_saver: the limit is 720 lines or the screen, whichever is shorter,
//     and among copies of equal height the lower bitrate wins.
//
// Either way the person's own cap (MaxHeight, MaxBitrateBPS) is a further
// ceiling, and the tighter of two ceilings is the one that holds. The video
// codecs and the HDR flag are facts about the device and are never touched:
// "best" on a screen without HDR still prefers an SDR copy, because a
// tone-mapped re-encode (#886, ADR-0125) only approximates the SDR grade.

// Quality is the person's preferred trade between picture and bytes.
type Quality string

// The three qualities. Stable: a client stores and sends the string.
const (
	QualityFit       Quality = "fit"
	QualityBest      Quality = "best"
	QualityDataSaver Quality = "data_saver"
)

// DataSaverMaxHeight is the data saver's ceiling, in lines.
const DataSaverMaxHeight = 720

// Preference is a person's playback quality, as the client resolved it: the
// user's synced default with this device's override laid over it. The zero
// value is "fit, no cap", which is exactly the behaviour before #887.
type Preference struct {
	Quality       Quality
	MaxHeight     int64
	MaxBitrateBPS int64
}

// Valid reports whether q is one of the three qualities, or empty (fit).
func (q Quality) Valid() bool {
	switch q {
	case "", QualityFit, QualityBest, QualityDataSaver:
		return true
	default:
		return false
	}
}

// label is how a reason names the quality: empty for fit, which is the
// unremarkable default and would only clutter a sentence.
func (q Quality) label() string {
	switch q {
	case QualityBest:
		return "best quality"
	case QualityDataSaver:
		return "data saver"
	default:
		return ""
	}
}

// Height is the height limit the preference leaves of a screen's screenHeight
// (zero: no limit stated). Selection, the planner and the leg all read it, so a
// copy chosen for its picture is not then scaled down to the screen.
func (p Preference) Height(screenHeight int64) int64 {
	switch p.Quality {
	case QualityBest:
		return p.MaxHeight
	case QualityDataSaver:
		return tighter(tighter(screenHeight, DataSaverMaxHeight), p.MaxHeight)
	default:
		return tighter(screenHeight, p.MaxHeight)
	}
}

// Bound is a registered device's profile with the preference laid over it:
// the limits the planner reads. The planner holds a picture to the device's
// width and height as a pair, so the width moves with the height. It scales in
// proportion when the device stated both, and when it stated no width it is
// the 16:9 frame of the new height. A height the preference lifts entirely
// lifts the width with it. When the preference leaves the height as it was,
// the profile is returned untouched.
func (p Preference) Bound(d DeviceProfile) DeviceProfile {
	h := p.Height(d.MaxHeight)
	if h == d.MaxHeight {
		return d
	}
	switch {
	case h <= 0:
		d.MaxWidth = 0
	case d.MaxWidth > 0 && d.MaxHeight > 0:
		d.MaxWidth = d.MaxWidth * h / d.MaxHeight
	default:
		d.MaxWidth = h * 16 / 9
	}
	d.MaxHeight = h
	return d
}

// Apply is fit with the preference laid over it: the limits selection ranks
// against. The bitrate cap steers the choice only, as max_bitrate_bps always
// has (ADR-0123): the repackager does not cap a stream.
func (p Preference) Apply(fit Fit) Fit {
	fit.MaxHeight = p.Height(fit.MaxHeight)
	fit.MaxBitrateBPS = tighter(fit.MaxBitrateBPS, p.MaxBitrateBPS)
	fit.Quality = p.Quality
	return fit
}

// tighter is the lower of two limits, where zero is no limit.
func tighter(a, b int64) int64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	default:
		return min(a, b)
	}
}
