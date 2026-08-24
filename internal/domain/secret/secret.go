// Package secret holds the redacting credential type.
//
// # Why it is a domain leaf package rather than part of providers
//
// It began in internal/providers, where the only credentials were indexer and
// download-client API keys. It moved when a SECOND kind of credential appeared
// on the other side of the layering: a release candidate's source URI is
// frequently a magnet carrying a private tracker's passkey (§63), and
// ReleaseCandidate lives in internal/domain/acquisition — which providers
// imports, so it cannot import providers back.
//
// The alternative was a second redacting type in the domain with the same four
// methods. That is the arrangement where one of them gains a print path the
// other does not, silently, and the leak is in whichever half nobody updated.
// One implementation, imported by both, is the only version of this that stays
// true. providers.Secret is now an ALIAS to Value, so no call site moved.
//
// This package imports nothing but the standard library and is safe for
// internal/domain/** under ADR-0006/0007.
package secret

import (
	"encoding/json"
	"log/slog"
)

// Redacted is what a credential renders as everywhere except Reveal.
//
// A fixed string rather than a length-preserving mask: "****" of the right
// length tells an attacker the key length, and tells an operator nothing they
// can act on.
const Redacted = "[redacted]"

// Value is a credential that cannot be printed by accident.
//
// # Why a type rather than care
//
// An indexer API key reaching a log line is a real leak, and this repository is
// public with a permanent git history behind it. "Remember not to log it" is a
// control that works until the day somebody adds slog.Any("config", cfg) while
// debugging something else — at which point every provider's credential is in
// the log, and nothing goes red to say so.
//
// So the plaintext is unreachable by every mechanism that prints things:
//
//	fmt %v / %s  -> String()
//	slog          -> LogValue()
//	encoding/json -> MarshalJSON()
//
// Getting the real value out requires Reveal, which is a named method that
// greps cleanly. The point is not that Reveal is hard to call — it is that
// calling it is a decision somebody made on purpose and a reviewer can find.
type Value string

// String redacts. This covers fmt's %v and %s, and errors built with %v.
func (s Value) String() string { return Redacted }

// LogValue redacts for slog, including when a whole struct is logged with
// slog.Any.
func (s Value) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON redacts, so a credential cannot reach an API response even if a
// wire type accidentally embeds the configuration rather than a view of it.
//
// It marshals to the redaction rather than to null or an empty string so that
// "there is a credential and you are not being shown it" and "there is no
// credential" stay tellable apart — which is exactly what an operator debugging
// a 401 needs to know.
func (s Value) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// UnmarshalJSON accepts a plain string, so configuration and fixtures read
// naturally.
func (s *Value) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*s = Value(raw)
	return nil
}

// Reveal returns the plaintext.
//
// Every call site is a place a credential leaves this package. There should be
// few, they should all be inside a provider implementation about to put the
// value on a wire, and `grep -rn 'Reveal()'` should be a short and boring list.
func (s Value) Reveal() string { return string(s) }

// IsZero reports whether a credential was supplied at all.
//
// Needed because String() redacts: `if cfg.APIKey == ""` still works on the
// underlying type, but a reader seeing Redacted everywhere reasonably wonders
// whether emptiness is observable. It is, and this is the name for it.
func (s Value) IsZero() bool { return s == "" }

// Compile-time proof that the redactions are wired. A type that silently stops
// implementing slog.LogValuer starts logging its plaintext, and nothing else
// would notice.
var (
	_ slog.LogValuer = Value("")
	_ json.Marshaler = Value("")
)
