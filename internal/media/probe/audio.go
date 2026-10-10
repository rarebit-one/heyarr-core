package probe

import "golang.org/x/text/language"

// AudioForLanguages selects main audio in preference order. The ordinal is
// relative to audio streams, matching ffmpeg's 0:a:N and not its absolute index.
// Missing preferred languages keep the original main audio, including foreign
// originals; a commentary track never substitutes for the preferred dialogue.
func (r Result) AudioForLanguages(preferred []string) (Stream, int, bool) {
	ordinal := 0
	fallback, fallbackOrdinal, found := Stream{}, 0, false
	for _, s := range r.Streams {
		if s.Type != "audio" {
			continue
		}
		if !found || (fallback.Commentary && !s.Commentary) {
			fallback, fallbackOrdinal, found = s, ordinal, true
		}
		ordinal++
	}
	for _, wanted := range preferred {
		target, err := language.Parse(wanted)
		if err != nil {
			continue
		}
		base, _, _ := target.Raw()
		if base.String() == "und" {
			continue
		}
		ordinal = 0
		for _, s := range r.Streams {
			if s.Type != "audio" {
				continue
			}
			tag, err := language.Parse(s.Language)
			actual, _, _ := tag.Raw()
			if err == nil && actual == base && !s.Commentary {
				return s, ordinal, true
			}
			ordinal++
		}
	}
	return fallback, fallbackOrdinal, found
}
