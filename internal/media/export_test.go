package media

// Seams exported to the external test package only. Everything a real
// executable can express is tested with a real executable in a t.TempDir
// instead; these cover what one cannot — see media.NoToolchain for the case
// that had to be exported properly rather than kept here.

// WithLookupResult simulates a PATH where each named tool resolves to the given
// path. An empty path means that tool is not on PATH.
func WithLookupResult(ffmpegPath, ffprobePath string) Options {
	return Options{LookPath: func(name string) (string, error) {
		switch {
		case name == CapabilityFFprobe && ffprobePath != "":
			return ffprobePath, nil
		case name == CapabilityFFmpeg && ffmpegPath != "":
			return ffmpegPath, nil
		}
		return "", &notFoundError{name: name}
	}}
}

// ParseVersionForTest exposes the banner parser, which is the piece most likely
// to meet a build whose banner nobody anticipated.
func ParseVersionForTest(banner string) (string, error) { return parseVersion(banner) }
