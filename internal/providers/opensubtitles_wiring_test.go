package providers

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The OpenSubtitles kind and its AuthTokenBasic scheme are wired together
// (ADR-0085): the first provider that needs two secrets, an api-key AND a
// username+password login. These assert the wiring the adapter depends on — the
// kind parses, defaults to the subtitle capability, needs a credential, needs no
// endpoint, and declares the two-secret scheme — and that the scheme resolves,
// refuses an incomplete block, and never leaks either secret.

func TestOpenSubtitlesKindWiring(t *testing.T) {
	k, err := ParseKind("opensubtitles")
	if err != nil {
		t.Fatalf("ParseKind(opensubtitles): %v", err)
	}
	if k != KindOpenSubtitles {
		t.Fatalf("parsed %q, want %q", k, KindOpenSubtitles)
	}

	caps := DefaultCapabilities(KindOpenSubtitles)
	if len(caps) != 1 || caps[0] != CapabilitySubtitle {
		t.Errorf("default capabilities = %v, want [subtitle]", caps)
	}
	if !needsCredential(KindOpenSubtitles) {
		t.Error("opensubtitles must need a credential — it 401s without an api-key")
	}
	if needsEndpoint(KindOpenSubtitles) {
		t.Error("opensubtitles must not need an endpoint — it has a well-known base URL")
	}

	// The subtitle capability is in the stable ordered set, or it never routes.
	found := false
	for _, c := range Capabilities() {
		if c == CapabilitySubtitle {
			found = true
		}
	}
	if !found {
		t.Error("CapabilitySubtitle is missing from the ordered Capabilities() set")
	}
}

func TestOpenSubtitlesAuthSchemeIsTokenBasic(t *testing.T) {
	if got := AuthSchemeOf(KindOpenSubtitles); got != AuthTokenBasic {
		t.Fatalf("AuthSchemeOf(opensubtitles) = %q, want %q", got, AuthTokenBasic)
	}
	// The scheme is in the stable set, so AuthSchemes()-driven validation knows it.
	found := false
	for _, s := range AuthSchemes() {
		if s == AuthTokenBasic {
			found = true
		}
	}
	if !found {
		t.Error("AuthTokenBasic is missing from AuthSchemes()")
	}
}

func TestOpenSubtitlesCredentialRoundTrips(t *testing.T) {
	resolved := mustResolveOne(t, Entry{
		Name: "opensubtitles", Type: "opensubtitles",
		Credential: &CredentialEntry{
			Token:    "an-api-key",
			Username: "a-user",
			Password: "a-password",
		},
	})

	token, user, pass, ok := resolved.Credential.TokenBasic()
	if !ok {
		t.Fatalf("an opensubtitles provider must resolve to a token-basic credential, got scheme %q",
			resolved.Credential.Scheme())
	}
	if token.Reveal() != "an-api-key" {
		t.Errorf("api-key = %q, want an-api-key", token.Reveal())
	}
	if user != "a-user" {
		t.Errorf("username = %q, want a-user", user)
	}
	if pass.Reveal() != "a-password" {
		t.Errorf("password = %q, want a-password", pass.Reveal())
	}

	// The other schemes' accessors must refuse it, so a caller cannot read a
	// token-basic credential as a bare token or basic pair.
	if _, ok := resolved.Credential.Token(); ok {
		t.Error("Token() answered a token-basic credential")
	}
	if _, _, ok := resolved.Credential.Basic(); ok {
		t.Error("Basic() answered a token-basic credential")
	}
}

func TestOpenSubtitlesCredentialRefusals(t *testing.T) {
	const leak = "SECRET-DO-NOT-LEAK"
	for _, tc := range []struct {
		name    string
		entry   Entry
		wantErr []string
	}{
		{
			name: "missing api-key",
			entry: Entry{
				Name: "opensubtitles", Type: "opensubtitles",
				Credential: &CredentialEntry{Username: "u", Password: Secret(leak)},
			},
			wantErr: []string{"opensubtitles", "credential.token"},
		},
		{
			name: "missing password",
			entry: Entry{
				Name: "opensubtitles", Type: "opensubtitles",
				Credential: &CredentialEntry{Token: Secret(leak), Username: "u"},
			},
			wantErr: []string{"opensubtitles", "credential.password"},
		},
		{
			name: "api_key shorthand cannot express two secrets",
			entry: Entry{
				Name: "opensubtitles", Type: "opensubtitles",
				APIKey: Secret(leak),
			},
			wantErr: []string{"opensubtitles", "username", "password"},
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
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("a refusal quoted the credential: %v", err)
			}
		})
	}
}

// Both secrets and the username must be withheld from every printing mechanism —
// the second secret (the api-key) is the new surface AuthTokenBasic adds, and a
// redaction that covered the password but printed the api-key would be a
// half-leak no test of the single-secret schemes would catch.
func TestTokenBasicCredentialRedacts(t *testing.T) {
	const (
		apiKey   = "apikey-DO-NOT-LEAK"
		username = "user-DO-NOT-LEAK"
		password = "password-DO-NOT-LEAK"
	)
	cred := TokenBasicCredential(Secret(apiKey), username, Secret(password))

	leaks := func(t *testing.T, where, output string) {
		t.Helper()
		for _, secret := range []string{apiKey, username, password} {
			if strings.Contains(output, secret) {
				t.Fatalf("%q reached %s: %s", secret, where, output)
			}
		}
	}

	leaks(t, "String()", cred.String())
	leaks(t, "fmt %v", fmt.Sprintf("%v", cred))
	leaks(t, "fmt %+v", fmt.Sprintf("%+v", cred))

	raw, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	leaks(t, "json", string(raw))

	leaks(t, "slog", cred.LogValue().String())
}
