package providers

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// What is asserted HERE is that a credential does not leak through the
// PROVIDER paths — configuration validation and provider construction. The
// redacting type itself is tested in internal/domain/secret, where it lives.
//
// The split matters: the type's own redaction and this package's discipline
// about where it puts the value are different claims, and a test for one is not
// evidence for the other.

// theSecret is a value that would be unmistakable in any output it reached.
const theSecret = "sk-live-DO-NOT-LEAK-8e91c4"

func TestBuildDoesNotLogCredentials(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	resolved, err := Validate([]Entry{{
		Name:     "an-indexer",
		Type:     string(KindTorznab),
		Endpoint: "https://indexer.invalid",
		APIKey:   Secret(theSecret),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(resolved, log, nil); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(buf.String(), theSecret) {
		t.Fatalf("registering a provider logged its credential:\n%s", buf.String())
	}
	// And the log is genuinely useful — a redaction test that passed against
	// an empty log would prove nothing.
	if !strings.Contains(buf.String(), "an-indexer") {
		t.Errorf("expected the provider to be logged at all, got:\n%s", buf.String())
	}
}

// A validation failure names the provider and the field. It must not quote the
// credential back, which is the tempting way to write "your api_key is wrong".
func TestValidationErrorsDoNotQuoteTheCredential(t *testing.T) {
	_, err := Validate([]Entry{{
		Name:     "an-indexer",
		Type:     string(KindTorznab),
		Endpoint: "not a url at all",
		APIKey:   Secret(theSecret),
	}})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), theSecret) {
		t.Fatalf("a validation error quoted the credential: %v", err)
	}
	if !strings.Contains(err.Error(), "an-indexer") {
		t.Errorf("the refusal should name the provider: %v", err)
	}
}

func TestSecretSurvivesLoggingAWholeStruct(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	entry := Entry{
		Name:     "an-indexer",
		Type:     string(KindTorznab),
		Endpoint: "https://indexer.invalid",
		APIKey:   Secret(theSecret),
	}

	// Every shape somebody reaches for at 2am.
	log.Info("debugging", "config", entry)
	log.Info("debugging", "key", entry.APIKey)
	log.Debug("debugging", slog.Any("providers", []Entry{entry}))
	log.Error("failed", "error", fmt.Errorf("using %v", entry.APIKey))

	if strings.Contains(buf.String(), theSecret) {
		t.Fatalf("the plaintext reached the log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), Redacted) {
		t.Errorf("expected the redaction in the log, got:\n%s", buf.String())
	}
}

// Reveal is the one way out, and it must actually work — a redaction that also
// redacted for the client would be a credential that authenticates nothing.
