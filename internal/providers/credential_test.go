package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// The credential a colon breaks.
//
// Deliberately a password that is ONLY plausible as a password: it contains a
// colon in the middle, so the pre-ADR-0031 parser cut it in half, and neither
// half is a credential anybody configured.
const passwordWithAColon = "hunter2:the-real-part-8e91c4"

// legacySplitCredential is a FROZEN COPY of the pre-ADR-0031 parser, kept here
// and nowhere else.
//
// It exists so the corruption this change fixes is REPRODUCED rather than
// described. A test that only asserted the new behaviour would pass equally
// against a codebase where the bug had never existed, which makes it a test
// nobody can tell is measuring anything. This one fails loudly if the old
// behaviour was not actually broken.
//
// Do not call it from anything but this file, and do not "fix" it.
func legacySplitCredential(secret string) (user, pass string) {
	for i := range len(secret) {
		if secret[i] == ':' {
			return secret[:i], secret[i+1:]
		}
	}
	return "transmission", secret
}

// The headline of #123: a colon in a password was a field separator, silently.
//
// Both halves are asserted in one test on purpose. Proving the new path
// preserves the password is only meaningful next to proof that the old one
// did not.
func TestAPasswordContainingAColonRoundTripsIntact(t *testing.T) {
	t.Run("the old behaviour mangled it", func(t *testing.T) {
		user, pass := legacySplitCredential(passwordWithAColon)

		// This is the bug, asserted as a fact rather than recalled as a story.
		if pass == passwordWithAColon {
			t.Fatal("the frozen legacy parser did not corrupt the password, " +
				"so the round-trip assertion below is measuring nothing")
		}
		if user != "hunter2" {
			t.Errorf("legacy username = %q; the old parser took everything "+
				"before the first colon", user)
		}
		if pass != "the-real-part-8e91c4" {
			t.Errorf("legacy password = %q; the old parser took everything "+
				"after the first colon", pass)
		}
	})

	t.Run("the typed credential preserves it", func(t *testing.T) {
		resolved := mustResolveOne(t, Entry{
			Name:     "a-download-client",
			Type:     string(KindTransmission),
			Endpoint: "http://transmission.invalid:9091/transmission/rpc",
			Credential: &CredentialEntry{
				Username: "heyarr",
				Password: Secret(passwordWithAColon),
			},
		})

		user, pass, ok := resolved.Credential.Basic()
		if !ok {
			t.Fatalf("a transmission provider must resolve to a basic credential, "+
				"got scheme %q", resolved.Credential.Scheme())
		}
		if user != "heyarr" {
			t.Errorf("username = %q, want %q", user, "heyarr")
		}
		// Byte for byte. The colon is a character in a password, not syntax.
		if pass.Reveal() != passwordWithAColon {
			t.Fatalf("the password was altered: got %q, want %q",
				pass.Reveal(), passwordWithAColon)
		}
	})
}

// A colon in a TOKEN was never ambiguous and must not become an error.
//
// Torznab's scheme is one opaque secret; there is no second field for a colon
// to be a separator between. This is the assertion that stops the fix for one
// scheme becoming a restriction on every other.
func TestAColonInATokenIsJustACharacter(t *testing.T) {
	resolved := mustResolveOne(t, Entry{
		Name:     "an-indexer",
		Type:     string(KindTorznab),
		Endpoint: "https://indexer.invalid",
		APIKey:   Secret(passwordWithAColon),
	})

	token, ok := resolved.Credential.Token()
	if !ok {
		t.Fatalf("a torznab provider must resolve to a token credential, got scheme %q",
			resolved.Credential.Scheme())
	}
	if token.Reveal() != passwordWithAColon {
		t.Fatalf("the token was altered: got %q, want %q", token.Reveal(), passwordWithAColon)
	}
}

// The ambiguous legacy spelling is now REFUSED rather than guessed at.
//
// Heyarr cannot tell "user:pass, as the old convention meant it" from "a
// password containing a colon" — they are the same bytes. Guessing is what
// produced the silent corruption, so the answer is to ask, at startup, once.
func TestAnAmbiguousLegacyCredentialIsAStartupError(t *testing.T) {
	_, err := Validate([]Entry{{
		Name:     "a-download-client",
		Type:     string(KindTransmission),
		Endpoint: "http://transmission.invalid:9091/transmission/rpc",
		APIKey:   Secret(passwordWithAColon),
	}})
	if err == nil {
		t.Fatal("expected a refusal — guessing here is the bug #123 is about")
	}
	for _, want := range []string{"a-download-client", "api_key", "colon", "credential:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q, said: %v", want, err)
		}
	}
	// And it must not quote the value back. A startup error ends up in a log.
	if strings.Contains(err.Error(), passwordWithAColon) {
		t.Fatalf("the refusal quoted the credential: %v", err)
	}
}

// Every provider that had a single credential keeps working, unchanged.
//
// This is the back-compatibility assertion. It is a table of the configuration
// shapes that existed BEFORE this change, each asserted to resolve to exactly
// what it resolved to before — including the defaulted "transmission"
// username, which is the behaviour #102 shipped and an operator may be relying
// on without knowing it.
func TestExistingSingleCredentialConfigurationsAreUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name         string
		entry        Entry
		wantScheme   AuthScheme
		wantUsername string
		wantSecret   string
	}{
		{
			name: "a torznab indexer with api_key",
			entry: Entry{
				Name: "an-indexer", Type: "torznab",
				Endpoint: "https://indexer.invalid", APIKey: "sk-live-8e91c4",
			},
			wantScheme: AuthToken,
			wantSecret: "sk-live-8e91c4",
		},
		{
			// The pre-#123 default, preserved exactly: an api_key with no
			// colon was the password, and the username was assumed.
			name: "a transmission client with a bare api_key password",
			entry: Entry{
				Name: "a-download-client", Type: "transmission",
				Endpoint: "http://transmission.invalid:9091/transmission/rpc",
				APIKey:   "hunter2",
			},
			wantScheme:   AuthBasic,
			wantUsername: "transmission",
			wantSecret:   "hunter2",
		},
		{
			// Authentication off is an ordinary supported deployment on a
			// trusted network, and must stay one.
			name: "a transmission client with no credential at all",
			entry: Entry{
				Name: "a-download-client", Type: "transmission",
				Endpoint: "http://transmission.invalid:9091/transmission/rpc",
			},
			wantScheme:   AuthBasic,
			wantUsername: "",
			wantSecret:   "",
		},
		{
			name: "a fake provider, which authenticates with nothing",
			entry: Entry{
				Name: "a-fake", Type: "fake",
				Capabilities: []string{"indexer"},
			},
			wantScheme: AuthNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := mustResolveOne(t, tc.entry)

			// assert_eq on an enum-like value, never a substring match.
			if got := resolved.Credential.Scheme(); got != tc.wantScheme {
				t.Fatalf("scheme = %q, want %q", got, tc.wantScheme)
			}

			switch tc.wantScheme {
			case AuthToken:
				token, ok := resolved.Credential.Token()
				if !ok {
					t.Fatal("a token credential must answer Token()")
				}
				if token.Reveal() != tc.wantSecret {
					t.Errorf("token = %q, want %q", token.Reveal(), tc.wantSecret)
				}
				if _, _, basicOK := resolved.Credential.Basic(); basicOK {
					t.Error("a token credential must not answer Basic()")
				}
			case AuthBasic:
				user, pass, ok := resolved.Credential.Basic()
				if !ok {
					t.Fatal("a basic credential must answer Basic()")
				}
				if user != tc.wantUsername {
					t.Errorf("username = %q, want %q", user, tc.wantUsername)
				}
				if pass.Reveal() != tc.wantSecret {
					t.Errorf("password = %q, want %q", pass.Reveal(), tc.wantSecret)
				}
				if _, tokenOK := resolved.Credential.Token(); tokenOK {
					t.Error("a basic credential must not answer Token()")
				}
			case AuthNone:
				if !resolved.Credential.IsZero() {
					t.Error("a provider that authenticates with nothing has no credential")
				}
			}
		})
	}
}

// The scheme is declared by the KIND, not inferred from what was configured.
// That is the half of the decision that makes the shape checkable at startup.
func TestTheAuthSchemeIsDeclaredByTheKind(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		want AuthScheme
	}{
		{KindTorznab, AuthToken},
		{KindTransmission, AuthBasic},
		{KindFake, AuthNone},
	} {
		if got := AuthSchemeOf(tc.kind); got != tc.want {
			t.Errorf("AuthSchemeOf(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
	// Every kind has a scheme. A kind added without one would silently
	// authenticate with nothing, which is the quiet failure mode this whole
	// change is about.
	for _, k := range Kinds() {
		scheme := AuthSchemeOf(k)
		known := false
		for _, s := range AuthSchemes() {
			if s == scheme {
				known = true
			}
		}
		if !known {
			t.Errorf("kind %q declares scheme %q, which is not a scheme", k, scheme)
		}
	}
}

// A credential written in the wrong scheme's shape is refused BY NAME.
//
// The alternative is `token:` on a Transmission entry being silently ignored,
// which is the same class of quiet failure as the colon.
func TestACredentialInTheWrongShapeIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entry   Entry
		wantErr []string
	}{
		{
			name: "a token on a basic provider",
			entry: Entry{
				Name: "a-download-client", Type: "transmission",
				Endpoint:   "http://transmission.invalid:9091/transmission/rpc",
				Credential: &CredentialEntry{Token: Secret(passwordWithAColon)},
			},
			wantErr: []string{"a-download-client", "credential.token", "basic"},
		},
		{
			name: "a username and password on a token provider",
			entry: Entry{
				Name: "an-indexer", Type: "torznab",
				Endpoint: "https://indexer.invalid",
				Credential: &CredentialEntry{
					Username: "heyarr", Password: Secret(passwordWithAColon),
				},
			},
			wantErr: []string{"an-indexer", "credential.token", "token"},
		},
		{
			name: "a username with no password",
			entry: Entry{
				Name: "a-download-client", Type: "transmission",
				Endpoint:   "http://transmission.invalid:9091/transmission/rpc",
				Credential: &CredentialEntry{Username: "heyarr"},
			},
			wantErr: []string{"a-download-client", "credential.password"},
		},
		{
			name: "both spellings at once",
			entry: Entry{
				Name: "a-download-client", Type: "transmission",
				Endpoint:   "http://transmission.invalid:9091/transmission/rpc",
				APIKey:     "hunter2",
				Credential: &CredentialEntry{Username: "heyarr", Password: "hunter2"},
			},
			wantErr: []string{"a-download-client", "api_key", "credential"},
		},
		{
			name: "a credential on a provider that authenticates with nothing",
			entry: Entry{
				Name: "a-fake", Type: "fake",
				Capabilities: []string{"indexer"},
				APIKey:       Secret(passwordWithAColon),
			},
			wantErr: []string{"a-fake", "authenticates with nothing"},
		},
		{
			name: "an empty credential block on a provider that needs one",
			entry: Entry{
				Name: "an-indexer", Type: "torznab",
				Endpoint: "https://indexer.invalid", Credential: &CredentialEntry{},
			},
			wantErr: []string{"an-indexer", "a credential is required"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Validate([]Entry{tc.entry})
			if err == nil {
				t.Fatal("expected a refusal")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal should mention %q, said: %v", want, err)
				}
			}
			// No refusal may quote a credential back — it ends up in a log.
			if strings.Contains(err.Error(), passwordWithAColon) {
				t.Fatalf("a refusal quoted the credential: %v", err)
			}
		})
	}
}

// The credential must not reach a log, an error or a response — asserted by
// SEARCHING CAPTURED OUTPUT for the plaintext, never by reading the code
// (ADR-0025).
//
// The basic scheme doubles the surface: there are now two fields, and a
// redaction that covered the password and printed the username would be a
// half-leak that no test of Secret alone would catch.
func TestATypedCredentialIsRedactedEverywhereOutputGoes(t *testing.T) {
	const username = "heyarr-DO-NOT-LEAK-username"
	cred := BasicCredential(username, Secret(passwordWithAColon))

	leaks := func(t *testing.T, where, output string) {
		t.Helper()
		if strings.Contains(output, passwordWithAColon) {
			t.Fatalf("the password reached %s: %s", where, output)
		}
		// Half a credential is still a credential.
		if strings.Contains(output, username) {
			t.Fatalf("the username reached %s: %s", where, output)
		}
		if !strings.Contains(output, Redacted) {
			t.Errorf("expected the redaction in %s, got: %s", where, output)
		}
	}

	t.Run("fmt %v", func(t *testing.T) {
		leaks(t, "%v", fmt.Sprintf("%v", cred))
	})
	t.Run("fmt %s", func(t *testing.T) {
		//nolint:staticcheck // testing the verb, not the method
		leaks(t, "%s", fmt.Sprintf("%s", cred))
	})
	t.Run("fmt %+v on the whole struct", func(t *testing.T) {
		// The 2am shape: printing a Resolved to see what got parsed.
		resolved := mustResolveOne(t, Entry{
			Name: "a-download-client", Type: "transmission",
			Endpoint: "http://transmission.invalid:9091/transmission/rpc",
			Credential: &CredentialEntry{
				Username: username, Password: Secret(passwordWithAColon),
			},
		})
		leaks(t, "%+v", fmt.Sprintf("%+v", resolved))
	})
	t.Run("inside an error", func(t *testing.T) {
		err := fmt.Errorf("provider %q could not authenticate with %v", "a-download-client", cred)
		leaks(t, "an error message", err.Error())
	})
	t.Run("encoding/json", func(t *testing.T) {
		raw, err := json.Marshal(struct {
			Credential Credential `json:"credential"`
		}{Credential: cred})
		if err != nil {
			t.Fatal(err)
		}
		leaks(t, "a JSON body", string(raw))
		// The SCHEME survives, and should: it is not secret, and it is the one
		// thing an operator debugging a 401 actually wants.
		if !bytes.Contains(raw, []byte(`"basic"`)) {
			t.Errorf("the scheme should survive redaction, got %s", raw)
		}
	})
	t.Run("slog over a whole config", func(t *testing.T) {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		entry := Entry{
			Name: "a-download-client", Type: string(KindTransmission),
			Endpoint: "http://transmission.invalid:9091/transmission/rpc",
			Credential: &CredentialEntry{
				Username: username, Password: Secret(passwordWithAColon),
			},
		}
		resolved := mustResolveOne(t, entry)

		// Every shape somebody reaches for at 2am.
		log.Info("debugging", "config", entry)
		log.Info("debugging", "credential", resolved.Credential)
		log.Debug("debugging", slog.Any("providers", []Resolved{resolved}))
		log.Error("failed", "error", fmt.Errorf("using %v", resolved.Credential))

		leaks(t, "the log", buf.String())
		// A redaction test that passed against an empty log would prove
		// nothing.
		if !strings.Contains(buf.String(), "a-download-client") {
			t.Errorf("expected the provider to be logged at all, got:\n%s", buf.String())
		}
	})
}

// The registry's own Entry still redacts when the credential is written the
// SHORTHAND way, which is what most existing configurations use.
func TestTheShorthandSpellingStillRedacts(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	entry := Entry{
		Name: "an-indexer", Type: string(KindTorznab),
		Endpoint: "https://indexer.invalid", APIKey: Secret(passwordWithAColon),
	}
	resolved := mustResolveOne(t, entry)
	log.Info("debugging", "config", entry, "resolved", resolved)

	if strings.Contains(buf.String(), passwordWithAColon) {
		t.Fatalf("the plaintext reached the log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), Redacted) {
		t.Errorf("expected the redaction in the log, got:\n%s", buf.String())
	}
}

// A Credential is not comparable to plaintext by accident, and emptiness stays
// observable despite String() redacting.
func TestCredentialEmptinessIsObservable(t *testing.T) {
	if !NoCredential().IsZero() {
		t.Error("a provider that authenticates with nothing has no credential")
	}
	if !BasicCredential("", "").IsZero() {
		t.Error("an unauthenticated basic provider has no credential")
	}
	if BasicCredential("heyarr", "hunter2").IsZero() {
		t.Error("a configured basic credential is not zero")
	}
	if TokenCredential("sk-live").IsZero() {
		t.Error("a configured token is not zero")
	}
	// The zero value is AuthNone rather than the empty string, so a Credential
	// nobody built still answers a scheme somebody can switch on.
	var zero Credential
	if zero.Scheme() != AuthNone {
		t.Errorf("the zero credential's scheme = %q, want %q", zero.Scheme(), AuthNone)
	}
}

func mustResolveOne(t *testing.T, e Entry) Resolved {
	t.Helper()
	resolved, err := Validate([]Entry{e})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolved %d providers, want 1", len(resolved))
	}
	return resolved[0]
}
