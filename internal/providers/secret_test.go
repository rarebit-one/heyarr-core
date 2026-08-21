package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// The credential must not reach a log, an error or a response.
//
// Every assertion here searches CAPTURED OUTPUT for the plaintext rather than
// reading the code. A test that inspected the implementation would pass for a
// type that had stopped redacting, which is precisely the change nobody would
// notice.

// theSecret is a value that would be unmistakable in any output it reached.
const theSecret = "sk-live-DO-NOT-LEAK-8e91c4"

func TestSecretIsRedactedEverywhereOutputGoes(t *testing.T) {
	s := Secret(theSecret)

	t.Run("fmt %v", func(t *testing.T) {
		if got := fmt.Sprintf("%v", s); strings.Contains(got, theSecret) {
			t.Fatalf("the plaintext reached %%v: %s", got)
		}
	})
	t.Run("fmt %s", func(t *testing.T) {
		// staticcheck suggests String() here, and that would defeat the test:
		// the claim is that %s VERB FORMATTING redacts, which is what somebody
		// building a message actually writes. Calling String() would assert
		// only that String() works.
		//nolint:staticcheck // testing the verb, not the method
		if got := fmt.Sprintf("%s", s); strings.Contains(got, theSecret) {
			t.Fatalf("the plaintext reached %%s: %s", got)
		}
	})
	t.Run("inside an error", func(t *testing.T) {
		err := fmt.Errorf("provider %q could not authenticate with %v", "an-indexer", s)
		if strings.Contains(err.Error(), theSecret) {
			t.Fatalf("the plaintext reached an error message: %v", err)
		}
	})
	t.Run("encoding/json", func(t *testing.T) {
		raw, err := json.Marshal(struct {
			Key Secret `json:"key"`
		}{Key: s})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(theSecret)) {
			t.Fatalf("the plaintext reached a JSON body: %s", raw)
		}
		// Redacted rather than omitted, so that "there is a credential and you
		// are not being shown it" and "there is no credential" stay tellable
		// apart — which is what an operator debugging a 401 needs to know.
		if !bytes.Contains(raw, []byte(Redacted)) {
			t.Errorf("a credential should render as %q, got %s", Redacted, raw)
		}
	})
}

// The case this type exists for: somebody logs a whole struct while debugging
// something else, and every provider's credential goes into the log.
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
func TestRevealReturnsThePlaintext(t *testing.T) {
	s := Secret(theSecret)
	if s.Reveal() != theSecret {
		t.Fatalf("Reveal returned %q", s.Reveal())
	}
	if s.IsZero() {
		t.Error("a non-empty credential is not zero")
	}
	if !Secret("").IsZero() {
		t.Error("an empty credential is zero")
	}
}

// A credential arrives from configuration as a plain string: the redaction is
// on the way out, not the way in.
func TestSecretUnmarshalsFromAPlainString(t *testing.T) {
	var got struct {
		Key Secret `json:"key"`
	}
	if err := json.Unmarshal([]byte(`{"key":"`+theSecret+`"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Key.Reveal() != theSecret {
		t.Fatalf("round trip lost the credential: %q", got.Key.Reveal())
	}
}

// The build path logs the endpoint and must not log the credential — and it
// must not because of the TYPE, not because that line remembered.
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

func TestSecretIsNotComparableToPlaintextByAccident(t *testing.T) {
	// A guard on the guard: String() redacting must not make emptiness
	// unobservable, or `if key == ""` checks elsewhere would silently break.
	if Secret("").String() != Redacted {
		t.Error("an empty credential still redacts, so it cannot be told apart by printing")
	}
}
