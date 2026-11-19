package hls

import "github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"

// SpecFor is the ffmpeg run a session would start at segment k.
func (s *Session) SpecFor(k int) ffmpeg.StreamSpec { return s.spec(k) }
