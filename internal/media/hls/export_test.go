package hls

import (
	"context"

	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
)

// SpecFor is the ffmpeg run a session would start at segment k.
func (s *Session) SpecFor(k int) ffmpeg.StreamSpec { return s.spec(k) }

// Await returns once ok holds. It checks ok again only when the session
// changes — a segment made, a run moving on, pausing or exiting — never on a
// timer, so a slow machine is slower and not a failure.
func (s *Session) Await(ctx context.Context, ok func() bool) error {
	for {
		// Take the channel before asking: a change between the two closes it.
		s.mu.Lock()
		ch := s.changed
		s.mu.Unlock()
		if ok() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}
