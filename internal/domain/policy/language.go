package policy

// Language defaults (§62, #558).
//
// A household has a preferred audio language, and the blunt way to express it
// is a bespoke profile: the deployed `everyday-en` was a clone of `everyday`
// with two hand-written prefer rules (bonus the wanted language, penalise the
// dubs) that every source then had to be repointed to. This turns that pattern
// into configuration: a LanguageDefault renders to the SAME §62 prefer rules,
// applied to the seeded video profiles, so language preference is one config
// value rather than a profile an operator clones and repoints.
//
// # Why prefer, never accept
//
// A profile that GATES on language rejects every release whose language the
// indexer could not determine — and a torrent title usually cannot be trusted
// to state one. That is #129's fail-closed rule: an accept gate whose attribute
// is undetermined does not pass, which is exactly why the video profiles gate
// on resolution and nothing softer. A language ACCEPT gate would therefore
// starve acquisition of every untagged (but perfectly good) English release.
// Stated as PREFER rules instead, a confirmed wanted-language release outscores
// a confirmed foreign one, an untagged release sits neutrally between them and
// is still fully acquirable, and nothing is ever rejected FOR its language.

// LanguageDefault is a household's audio-language preference, rendered into
// ordinary prefer rules so scoring stays one model (§62/§63).
//
// The zero value is DISABLED: it produces no rules and leaves scoring exactly
// as it is, which is what an operator who does not care about language gets.
type LanguageDefault struct {
	// Prefer is the ISO-639-1 code of the wanted audio language, e.g. "en".
	// Empty disables the whole default — there is no such thing as a language
	// preference that does not name a language.
	Prefer string
	// Weight is the bonus a release confirmed to be in Prefer scores. A prefer
	// weight, so it is never a gate; zero simply means "do not reward the
	// wanted language", which is legal (the penalty below may still steer).
	Weight int
	// Foreign is the set of languages to steer AWAY from — the dubs the
	// household does not want. A release confirmed to be one of these is
	// penalised; every OTHER language, and an undetermined one, is left neutral.
	Foreign []string
	// Penalty is how far a Foreign release is pushed down. Stored positive and
	// applied as a negative weight, because `penalty: 100` reads more plainly
	// than `weight: -100` for the same thing.
	Penalty int
}

// Enabled reports whether this default names a language to prefer. A default
// with no Prefer produces nothing, even if a Foreign list is set: penalising
// every dub without saying what you DO want is not a preference anyone means.
func (d LanguageDefault) Enabled() bool { return d.Prefer != "" }

// PreferRules renders the default as §62 prefer rules: a bonus for the wanted
// language, and a penalty for the unwanted ones. Empty when disabled.
func (d LanguageDefault) PreferRules() []Rule {
	if !d.Enabled() {
		return nil
	}
	var rules []Rule
	if d.Weight > 0 {
		rules = append(rules, Rule{
			Attribute: AttrLanguage, Op: OpEq, Value: Text(d.Prefer), Weight: d.Weight,
		})
	}
	if len(d.Foreign) > 0 && d.Penalty > 0 {
		rules = append(rules, Rule{
			Attribute: AttrLanguage, Op: OpIn, Value: Texts(d.Foreign...), Weight: -d.Penalty,
		})
	}
	return rules
}

// mentionsLanguage reports whether any section already asserts language. It is
// the signal that a profile has its OWN opinion, which the server default must
// defer to (a profile-level override of a server-level default, §558's layer 2
// over layer 1).
func (p Profile) mentionsLanguage() bool {
	for _, sr := range p.Rules() {
		if sr.Rule.Attribute == AttrLanguage {
			return true
		}
	}
	return false
}

// WithLanguageDefault returns p unchanged when it already mentions language —
// the author's explicit choice wins — otherwise a copy with the default's
// prefer rules appended. Pure: the receiver is not mutated, so a shared
// Defaults() slice is safe to pass through it.
func (p Profile) WithLanguageDefault(d LanguageDefault) Profile {
	if !d.Enabled() || p.mentionsLanguage() {
		return p
	}
	add := d.PreferRules()
	if len(add) == 0 {
		return p
	}
	out := p
	out.Prefer = append(append([]Rule{}, p.Prefer...), add...)
	return out
}

// WithLanguageDefaults applies the default across a set, touching only the
// VIDEO profiles. A captured article (`published`) or a fetched caption
// (`subtitle`) has no audio language, and giving it a rule about one would make
// it reject or mis-score content it is the whole point of admitting. A profile
// is treated as a video profile when any of its rules references a video-only
// attribute; `published` (source only) and `subtitle` (size only) reference
// none, so they are left alone.
func WithLanguageDefaults(profiles []Profile, d LanguageDefault) []Profile {
	if !d.Enabled() {
		return profiles
	}
	out := make([]Profile, len(profiles))
	for i, p := range profiles {
		if isVideoProfile(p) {
			out[i] = p.WithLanguageDefault(d)
		} else {
			out[i] = p
		}
	}
	return out
}

// isVideoProfile reports whether a profile judges video — the signal that audio
// language is a meaningful thing to prefer for it.
func isVideoProfile(p Profile) bool {
	for _, sr := range p.Rules() {
		switch sr.Rule.Attribute {
		case AttrResolution, AttrVideoCodec, AttrHDR, AttrAudioCodec, AttrAudioChannels:
			return true
		}
	}
	return false
}
