package providers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// Credentials are TYPED BY THE PROVIDER'S DECLARED AUTH SCHEME (ADR-0031).
//
// # The problem this replaces
//
// A provider Entry carried one credential field, `api_key`. Torznab wants one
// opaque token, which fits. Transmission's RPC is HTTP basic auth and wants a
// username AND a password, which does not — so #102 packed both into the one
// field as "user:pass", defaulting the username to "transmission".
//
// That was a CONVENTION, and a convention fails silently. A password
// containing a colon — "p:ssw0rd" — was split at the colon, so Heyarr
// authenticated as user "p" with password "ssw0rd", got a 401, and reported an
// unreachable download client. Nothing was wrong with the configuration and
// nothing said so.
//
// # Why not a `username` field on Entry
//
// Because it would be empty for every provider but this one. Torznab, and
// every metadata provider likely to follow, authenticates with a bare token.
// Adding a column that one integration uses and the rest leave blank is how a
// registry accretes per-service fields, which is precisely what §59's
// centralised provider configuration exists to prevent.
//
// # What replaces it
//
// The SHAPE of a credential is a property of the provider's AUTH SCHEME, and
// the scheme is part of the kind's declaration rather than implied by how a
// string happens to be parsed. There is still exactly one credential concept
// in the registry; it simply has more than one shape, and which shape applies
// is declared rather than guessed.
//
// A configuration that does not match the declared scheme is a startup error
// naming the provider, the field and the scheme — ADR-0025's rule that a
// mistake somebody can fix in ten seconds belongs at startup, not at the first
// search.

// AuthScheme is how a provider proves who it is.
//
// It is spelled in the same structured lower-case idiom as a Capability, for
// the same reason: a reader who has met one has learnt the other.
type AuthScheme string

const (
	// AuthNone is a provider that authenticates with nothing. The fake, and
	// any future in-process provider.
	AuthNone AuthScheme = "none"
	// AuthToken is one opaque secret, sent however the protocol says — a query
	// parameter for Torznab, a header elsewhere. The overwhelmingly common
	// case, and the one `api_key` was named for.
	AuthToken AuthScheme = "token"
	// AuthBasic is a username and a password, per RFC 7617. Transmission's RPC
	// is the first, and it is why this type exists.
	AuthBasic AuthScheme = "basic"
)

// AuthSchemes lists every scheme, in a stable order.
func AuthSchemes() []AuthScheme { return []AuthScheme{AuthNone, AuthToken, AuthBasic} }

// AuthSchemeOf is the scheme a KIND declares.
//
// This is the "part of the provider's declaration" half of the decision: the
// scheme is a property of the protocol, known before any configuration is
// read, so validation can say what shape it expected rather than discovering
// the shape from what it was given.
func AuthSchemeOf(k Kind) AuthScheme {
	switch k {
	case KindTorznab, KindNewznab, KindTVDB, KindSABnzbd:
		// TheTVDB v4 authenticates with one API key it exchanges for a bearer
		// token — one opaque secret, sent however the protocol says, which is
		// exactly AuthToken (M12). SABnzbd is the same shape: one api_key, sent
		// as a query parameter — how the token goes on the wire is the client's
		// business, and the CREDENTIAL an operator supplies is one opaque secret.
		return AuthToken
	case KindTransmission, KindQBittorrent:
		// qBittorrent's Web API is a username+password login (it mints a session
		// cookie from them); the CREDENTIAL an operator supplies is the same
		// basic pair, and how it goes on the wire is the client's business.
		return AuthBasic
	case KindHTTP, KindPodcast:
		// A plain-HTTP download fetches a public direct link, and a podcast RSS
		// feed is a public URL; both authenticate with nothing. A feed or link
		// behind a credential is a later scheme, not this slice — and AuthNone
		// means configuration refuses a credential here rather than accepting one
		// that would never be sent.
		return AuthNone
	case KindFake:
		return AuthNone
	default:
		return AuthNone
	}
}

// defaultUsername is the username a basic-scheme kind assumes when
// configuration supplies only a password.
//
// It exists for back-compatibility and for the ordinary case: Transmission's
// own default account is "transmission", and #102 already defaulted to it. An
// operator who was relying on that keeps working with no config change.
func defaultUsername(k Kind) string {
	switch k {
	case KindTransmission:
		return "transmission"
	case KindQBittorrent:
		// qBittorrent's default Web UI account is "admin"; an operator who left
		// it and supplied only a password keeps working with no config change.
		return "admin"
	default:
		return ""
	}
}

// Credential is a provider's credential, shaped by its scheme.
//
// The fields are unexported and reachable only through the scheme-appropriate
// accessor, so a caller cannot read a password out of a token provider or a
// token out of a basic one. That is the whole point: the compiler and the
// accessors carry what a naming convention used to carry.
//
// Like Secret, it redacts in fmt, in slog and in JSON. It is a distinct type
// from Secret rather than a struct containing one because a struct of Secrets
// would redact its members and still print its username, and a username is a
// credential half.
type Credential struct {
	scheme   AuthScheme
	username string
	secret   Secret
}

// NoCredential is the credential of a provider that authenticates with
// nothing. Distinct from a zero Credential only in intent; both are AuthNone.
func NoCredential() Credential { return Credential{scheme: AuthNone} }

// TokenCredential is one opaque secret.
func TokenCredential(token Secret) Credential {
	return Credential{scheme: AuthToken, secret: token}
}

// BasicCredential is a username and a password.
//
// The password is a Secret and the username is not, because the username is
// not independently sensitive — but both are withheld from every printing
// mechanism anyway (see LogValue), since a username is half of what an
// attacker reading a log needs.
func BasicCredential(username string, password Secret) Credential {
	return Credential{scheme: AuthBasic, username: username, secret: password}
}

// Scheme is which shape this credential has.
func (c Credential) Scheme() AuthScheme {
	if c.scheme == "" {
		return AuthNone
	}
	return c.scheme
}

// IsZero reports whether any credential was supplied at all.
//
// A basic credential with an empty password is zero even when a username was
// defaulted in: Transmission with authentication switched off is an ordinary
// supported deployment, and "there is a username because we made one up" must
// not read as "the operator configured authentication".
func (c Credential) IsZero() bool { return c.secret.IsZero() && c.username == "" }

// Token returns the opaque secret, and false if this is not a token
// credential.
//
// The boolean is not ceremony. A caller that read a token out of a basic
// credential would send half of an RFC 7617 pair as a bearer token and get a
// 401 that looks like a service fault.
func (c Credential) Token() (Secret, bool) {
	if c.Scheme() != AuthToken {
		return "", false
	}
	return c.secret, true
}

// Basic returns the username and password, and false if this is not a basic
// credential.
//
// An empty password with ok=true means authentication is configured off, which
// Transmission on a trusted network legitimately is.
func (c Credential) Basic() (username string, password Secret, ok bool) {
	if c.Scheme() != AuthBasic {
		return "", "", false
	}
	return c.username, c.secret, true
}

// String redacts. Covers %v, %s and errors built with %v.
func (c Credential) String() string { return Redacted }

// LogValue redacts for slog, including slog.Any over a whole struct.
//
// The SCHEME survives, because it is not secret and it is the one thing an
// operator reading a log about a 401 actually wants: "we authenticated as
// basic against something expecting a token" is a diagnosis. The username does
// not survive, because half a credential is still a credential.
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("scheme", string(c.Scheme())),
		slog.String("credential", Redacted),
	)
}

// MarshalJSON redacts, so a credential cannot reach an API response even if a
// wire type accidentally embeds the configuration rather than a view of it.
func (c Credential) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Scheme     AuthScheme `json:"scheme"`
		Credential string     `json:"credential"`
	}{Scheme: c.Scheme(), Credential: Redacted})
}

// Compile-time proof that the redactions are wired. A type that silently stops
// implementing slog.LogValuer starts logging its plaintext, and nothing else
// would notice.
var (
	_ slog.LogValuer = Credential{}
	_ json.Marshaler = Credential{}
	_ fmt.Stringer   = Credential{}
)

// CredentialEntry is the typed credential block as configuration writes it.
//
//	credential:
//	  username: heyarr        # basic only
//	  password: "p:ssw0rd"    # basic only — colons are just characters here
//
//	credential:
//	  token: "sk-live-..."    # token only
//
// Which keys are permitted is decided by the kind's declared scheme, and a key
// belonging to another scheme is refused by name. That refusal is the feature:
// the alternative is `token` on a Transmission entry being silently ignored,
// which is the same class of quiet failure this whole change is about.
type CredentialEntry struct {
	Username string `koanf:"username"`
	Password Secret `koanf:"password"`
	Token    Secret `koanf:"token"`
}

func (c CredentialEntry) isZero() bool {
	return c.Username == "" && c.Password.IsZero() && c.Token.IsZero()
}

// The configuration block redacts as a WHOLE, username included.
//
// Secret already covers the password and the token, and that was not enough: a
// log of the raw Entry printed `"username":"heyarr"` beside two redactions,
// which is half of an RFC 7617 pair handed over for free. This was caught by
// the log-scanning test rather than by review, which is the argument for
// scanning output instead of reading code.
//
// Nothing here is dropped rather than redacted, for Secret's reason: "there is
// a credential and you are not being shown it" and "there is no credential"
// must stay tellable apart by an operator debugging a 401.

// String redacts, covering %v and %+v on an Entry that holds one.
func (c CredentialEntry) String() string { return Redacted }

// LogValue redacts for slog, including slog.Any over a whole Entry.
func (c CredentialEntry) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON redacts, so the block cannot reach an API response or a
// JSON-handler log line.
func (c CredentialEntry) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// UnmarshalJSON accepts the object form, so configuration and fixtures read
// naturally despite MarshalJSON being asymmetric. The redaction is on the way
// OUT, not the way in.
func (c *CredentialEntry) UnmarshalJSON(b []byte) error {
	var raw struct {
		Username string `json:"username"`
		Password Secret `json:"password"`
		Token    Secret `json:"token"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	c.Username, c.Password, c.Token = raw.Username, raw.Password, raw.Token
	return nil
}

var (
	_ slog.LogValuer = CredentialEntry{}
	_ json.Marshaler = CredentialEntry{}
	_ fmt.Stringer   = CredentialEntry{}
)

// resolveCredential turns configuration into a typed credential, or refuses.
//
// Two spellings are accepted and they may not be mixed:
//
//   - `credential:` — the typed block, shaped by the declared scheme.
//   - `api_key:` — the pre-#123 shorthand, still the right thing to write for
//     a token provider, and still accepted as a lone password for a basic one.
//
// # The one deliberate break
//
// `api_key` holding a colon on a BASIC-scheme provider is now a startup error
// rather than a silent split. Heyarr cannot tell "user:pass, as #102's
// convention meant it" from "a password that contains a colon" — the two are
// the same bytes — so the only honest answers are to guess or to ask. Guessing
// is what produced the silent corruption; this asks, once, at startup, with
// the fix in the message.
func resolveCredential(name string, kind Kind, e Entry) (Credential, error) {
	scheme := AuthSchemeOf(kind)

	var block CredentialEntry
	if e.Credential != nil {
		block = *e.Credential
	}
	hasBlock := e.Credential != nil && !block.isZero()
	hasShorthand := !e.APIKey.IsZero()

	if hasBlock && hasShorthand {
		return Credential{}, fmt.Errorf(
			"provider %q: api_key and credential are two spellings of the same thing "+
				"and only one of them can be right — keep credential and delete api_key", name)
	}

	if scheme == AuthNone {
		if hasBlock || hasShorthand {
			return Credential{}, fmt.Errorf(
				"provider %q: a %s provider authenticates with nothing, so a credential "+
					"here would never be sent — remove it", name, kind)
		}
		return NoCredential(), nil
	}

	if hasBlock {
		return credentialFromBlock(name, kind, scheme, block)
	}
	if hasShorthand {
		return credentialFromShorthand(name, kind, scheme, e.APIKey)
	}

	// Nothing configured. Whether that is allowed is needsCredential's
	// question, asked by Validate, and it is a different question from this
	// one: an unauthenticated Transmission is ordinary, an unauthenticated
	// Torznab 401s on its first search.
	switch scheme {
	case AuthToken:
		return TokenCredential(""), nil
	case AuthBasic:
		return BasicCredential("", ""), nil
	case AuthNone:
		return NoCredential(), nil
	default:
		return NoCredential(), nil
	}
}

func credentialFromBlock(
	name string, kind Kind, scheme AuthScheme, block CredentialEntry,
) (Credential, error) {
	switch scheme {
	case AuthToken:
		if block.Username != "" || !block.Password.IsZero() {
			return Credential{}, fmt.Errorf(
				"provider %q: a %s provider takes a %s credential, so credential.token "+
					"is the only key it reads — username and password would never be sent",
				name, kind, scheme)
		}
		if block.Token.IsZero() {
			return Credential{}, fmt.Errorf(
				"provider %q: credential.token is empty, and a %s provider has nothing "+
					"else to authenticate with", name, kind)
		}
		return TokenCredential(block.Token), nil

	case AuthBasic:
		if !block.Token.IsZero() {
			return Credential{}, fmt.Errorf(
				"provider %q: a %s provider takes a %s credential — a username and a "+
					"password — so credential.token would never be sent", name, kind, scheme)
		}
		if block.Password.IsZero() {
			return Credential{}, fmt.Errorf(
				"provider %q: credential.username is set without credential.password, "+
					"which would authenticate as nobody", name)
		}
		username := strings.TrimSpace(block.Username)
		if username == "" {
			username = defaultUsername(kind)
		}
		// The password is taken EXACTLY as written. No splitting, no trimming:
		// a colon is a character in a password and so is a leading space, and
		// the previous convention's whole failure was treating one of them as
		// syntax.
		return BasicCredential(username, block.Password), nil

	case AuthNone:
		return NoCredential(), nil
	default:
		return NoCredential(), nil
	}
}

func credentialFromShorthand(
	name string, kind Kind, scheme AuthScheme, key Secret,
) (Credential, error) {
	switch scheme {
	case AuthToken:
		// Unchanged, and this is the common case: `api_key` is exactly the
		// right thing to write for a provider whose credential is one opaque
		// token. A colon in it is just a character.
		return TokenCredential(key), nil

	case AuthBasic:
		if strings.Contains(key.Reveal(), ":") {
			// The silent corruption, made loud. Note that the value is never
			// quoted back: this message must be safe in a log.
			return Credential{}, fmt.Errorf(
				"provider %q: api_key contains a colon, and for a %s provider that is "+
					"ambiguous — it could be a username and password, or a password that "+
					"happens to contain a colon, and guessing wrong authenticates as the "+
					"wrong user with a mangled password. Write it out instead:\n"+
					"    credential:\n      username: <user>\n      password: <password>",
				name, scheme)
		}
		return BasicCredential(defaultUsername(kind), key), nil

	case AuthNone:
		return NoCredential(), nil
	default:
		return NoCredential(), nil
	}
}
