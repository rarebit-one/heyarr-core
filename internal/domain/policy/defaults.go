package policy

// Defaults are the profiles a fresh Heyarr starts with.
//
// # Why there are any at all
//
// A DesiredItem must name a profile (M3-02), so a Heyarr with no profiles is
// one where the first interesting thing you can do requires authoring JSON
// against a vocabulary you have not read yet. The seeded set is not a
// recommendation about anyone's television; it is the difference between
// "want this" working out of the box and not.
//
// # Why these three
//
// They differ along the axis that actually matters, which is what `terminal`
// says — that is, when Heyarr stops looking:
//
//	living-room  a big screen. Terminal at 2160p remux: there is a ceiling
//	             and it is reachable, so the upgrade loop ends.
//	everyday     a laptop. Terminal at 1080p: a lower ceiling, reached sooner.
//	archival     keep the best there is. NO terminal at all, so it never stops
//	             looking — which is a real thing to want and the case that
//	             proves an absent `terminal` is legal rather than a modelling
//	             oversight.
//
// The third is there for that last reason as much as for its usefulness. A
// default set where every profile terminates would leave the "never finished"
// path unexercised by anything but a unit test.
//
// # These are values, not rows
//
// Seeding is persistence's job and happens at controller start, converging on
// the profile NAME. That way a restart does not duplicate them, and an
// operator who edits `living-room` keeps their edit instead of having it
// overwritten every morning by whatever this file happens to say.
func Defaults() []Profile {
	return []Profile{
		{
			Name: "living-room",
			Description: "A television. Accepts 1080p and up, prefers modern codecs " +
				"and HDR, and is finished at a 2160p remux.",
			Accept: []Rule{
				{Attribute: AttrResolution, Op: OpGTE, Value: Num(1080)},
				// A cam is never acceptable on a big screen, and saying so as
				// a gate rather than a large penalty means the rejection
				// reason names it (§63) instead of burying it in arithmetic.
				{Attribute: AttrSource, Op: OpNotIn, Value: Texts("cam", "telesync")},
			},
			Prefer: []Rule{
				{Attribute: AttrVideoCodec, Op: OpEq, Value: Text("hevc"), Weight: 20},
				{Attribute: AttrHDR, Op: OpEq, Value: Flag(true), Weight: 10},
				{Attribute: AttrAudioChannels, Op: OpGTE, Value: Num(6), Weight: 5},
				// A penalty, and the reason negative weights are legal: an
				// enormous file is not disqualifying, it is merely worse than
				// an equivalent smaller one.
				{Attribute: AttrSizeBytes, Op: OpGTE, Value: Num(64 << 30), Weight: -15},
			},
			Terminal: []Rule{
				{Attribute: AttrResolution, Op: OpGTE, Value: Num(2160)},
				{Attribute: AttrSource, Op: OpEq, Value: Text("remux")},
			},
		},
		{
			Name: "everyday",
			Description: "A laptop or a tablet. Accepts 720p and up and is finished " +
				"at 1080p — smaller files, reached sooner.",
			Accept: []Rule{
				{Attribute: AttrResolution, Op: OpGTE, Value: Num(720)},
				{Attribute: AttrSource, Op: OpNotIn, Value: Texts("cam", "telesync")},
			},
			Prefer: []Rule{
				{Attribute: AttrVideoCodec, Op: OpEq, Value: Text("hevc"), Weight: 15},
				{Attribute: AttrSizeBytes, Op: OpLTE, Value: Num(8 << 30), Weight: 10},
			},
			Terminal: []Rule{
				{Attribute: AttrResolution, Op: OpGTE, Value: Num(1080)},
			},
		},
		{
			Name: "archival",
			Description: "Keep the best there is. Never terminal: there is no " +
				"condition under which this profile stops looking for something better.",
			Accept: []Rule{
				{Attribute: AttrSource, Op: OpNotIn, Value: Texts("cam", "telesync", "workprint")},
			},
			Prefer: []Rule{
				{Attribute: AttrSource, Op: OpEq, Value: Text("remux"), Weight: 40},
				{Attribute: AttrResolution, Op: OpGTE, Value: Num(2160), Weight: 30},
				{Attribute: AttrHDR, Op: OpEq, Value: Flag(true), Weight: 10},
			},
			// No Terminal. Deliberate, and asserted.
		},
		{
			// The profile for content taken DIRECTLY from the feed that publishes
			// it — a captured web article, a podcast episode (ADR-0082). The other
			// three profiles are video profiles: their accept gate is a resolution
			// floor, which a document or an audio file has no answer for, so it
			// evaluates to `undetermined` and the asset is refused with a blob
			// sitting on disk. This one judges on the evidence such content
			// actually carries.
			Name: "published",
			Description: "Content taken directly from the source that publishes it — a " +
				"web article, a podcast episode. The published copy is definitive: accept " +
				"it and stop looking.",
			Accept: []Rule{
				// A cam/telesync/workprint is a leaked video and never what a feed
				// legitimately publishes; everything else — html, and any audio or
				// document source — is admitted. Stated as a gate, not a penalty, so
				// a rejection names the reason (§63).
				{Attribute: AttrSource, Op: OpNotIn, Value: Texts("cam", "telesync", "workprint")},
			},
			// No Prefer: there is no "better" copy of a page the feed handed over,
			// so nothing to rank one candidate above another by.
			Terminal: []Rule{
				// Any bytes at all is as good as it gets. A captured article is not
				// improved by capturing it again, so the want goes terminal the
				// moment it holds something — the upgrade loop must not re-examine
				// it forever (which, on the video profiles, is what `archival` does
				// on purpose and this must not do by accident).
				{Attribute: AttrSizeBytes, Op: OpGTE, Value: Num(1)},
			},
		},
	}
}
