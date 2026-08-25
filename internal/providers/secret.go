package providers

import "github.com/rarebit-one/heyarr-core/internal/domain/secret"

// Secret is a credential that cannot be printed by accident.
//
// An ALIAS rather than a new type, so that every existing call site — and
// every value already flowing through configuration, the catalogue and the
// provider implementations — keeps working unchanged, while there is exactly
// one implementation of the redaction.
//
// It lives in internal/domain/secret because credentials appear on both sides
// of the layering: providers imports internal/domain/acquisition, whose
// ReleaseCandidate carries a source URI that is frequently a magnet with a
// private tracker's passkey in it. See that package's doc for why a second
// redacting type here would be the version that eventually leaks.
type Secret = secret.Value

// Redacted is what a credential renders as everywhere except Reveal.
const Redacted = secret.Redacted
