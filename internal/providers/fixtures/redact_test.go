package fixtures

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The capture script's redactor, tested rather than trusted.
//
// redact() is the single most important function in scripts/capture-fixtures.sh:
// it is what stands between a real tracker passkey and a permanent public
// record. It is also seven sed expressions, which is exactly the kind of thing
// that has a typo in one of them and nobody notices until the wrong week.
//
// The scanner in this package is a SECOND line, over the committed corpus in
// CI. This tests the first one — because a mistake caught by the scanner has
// already been written to a file, and the whole design is that it never gets
// that far.

// runRedact pipes input through the capture script's redact function.
//
// The script guards its own dispatch on BASH_SOURCE, so sourcing it defines
// the functions without running the capture. That seam exists for this test.
func runRedact(t *testing.T, input string) string {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "capture-fixtures.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", "source "+script+" && redact")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sourcing the capture script failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestTheCaptureScriptRedactsEveryShape(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		secret string
	}{
		{
			"an api key in a query string",
			`{"path":"/api/v1/search?apikey=8f14e45fceea167a5a36dedd4bea2543&q=x"}`,
			"8f14e45fceea167a5a36dedd4bea2543",
		},
		{
			"an api key in a header",
			`{"headers":{"X-Api-Key":"deadbeefdeadbeefdeadbeefdeadbeef"}}`,
			"deadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			"a Transmission session id",
			`{"headers":{"X-Transmission-Session-Id":"AbCd1234EfGh5678IjKl"}}`,
			"AbCd1234EfGh5678IjKl",
		},
		{
			"credentials in a URL",
			`{"endpoint":"http://someone:hunter2@downloads.invalid:9091/transmission/rpc"}`,
			"hunter2",
		},
		{
			"a named tracker passkey",
			`{"announce":"https://tracker.invalid/announce?passkey=0123456789abcdef0123456789"}`,
			"0123456789abcdef0123456789",
		},
		{
			// The shape with nothing naming it, which is the easiest to miss.
			"a passkey as a bare path segment",
			`{"announce":"https://tracker.invalid/announce/0123456789abcdef0123456789"}`,
			"0123456789abcdef0123456789",
		},
		{
			"an Authorization header",
			`{"headers":{"Authorization":"Bearer sk-abcdefghijklmnopqrstuvwx"}}`,
			"sk-abcdefghijklmnopqrstuvwx",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runRedact(t, tc.input)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("the secret survived redaction:\n%s", got)
			}
			if !strings.Contains(got, Redacted) {
				t.Errorf("nothing was redacted at all:\n%s", got)
			}
		})
	}
}

// The redactor must not eat things that are not secrets. A redactor that
// destroys the response is one nobody uses, and the corpus is full of hashes
// that look like keys to a careless pattern.
func TestTheCaptureScriptKeepsWhatIsNotASecret(t *testing.T) {
	const keep = `{"hashString":"2b3c4d5e6f708192a3b4c5d6e7f80912a3b4c5d6",` +
		`"blob":"blake3:1111111111111111111111111111111111111111111111111111111111111111",` +
		`"name":"Some.Release.2160p.mkv","downloadDir":"/data/torrents/movies"}`
	got := runRedact(t, keep)
	for _, want := range []string{
		"2b3c4d5e6f708192a3b4c5d6e7f80912a3b4c5d6",
		"blake3:1111111111111111111111111111111111111111111111111111111111111111",
		"Some.Release.2160p.mkv",
		"/data/torrents/movies",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("redaction destroyed %q, which is not a credential:\n%s", want, got)
		}
	}
}

// The two halves have to agree. Anything the redactor produces must be clean to
// the scanner — otherwise a correct capture goes red in CI and somebody turns
// the guard off.
func TestWhatTheRedactorProducesIsCleanToTheScanner(t *testing.T) {
	const dirty = `{"path":"/api/v1/search?apikey=8f14e45fceea167a5a36dedd4bea2543",` +
		`"headers":{"X-Api-Key":"deadbeefdeadbeefdeadbeefdeadbeef",` +
		`"Authorization":"Bearer sk-abcdefghijklmnopqrstuvwx"},` +
		`"endpoint":"http://someone:hunter2@downloads.invalid:9091/rpc",` +
		`"announce":"https://tracker.invalid/announce?passkey=0123456789abcdef0123456789",` +
		`"announce2":"https://tracker.invalid/announce/0123456789abcdef0123456789"}`

	cleaned := runRedact(t, dirty)
	if findings := scanBytes("redacted.json", cleaned); len(findings) != 0 {
		t.Errorf("the scanner flags what the redactor produced, so a correct capture "+
			"would go red in CI:\n%v\n%s", findings, cleaned)
	}
	// And the sabotage in reverse: the UNredacted input must be flagged, or
	// this test proves nothing.
	if findings := scanBytes("dirty.json", dirty); len(findings) == 0 {
		t.Fatal("the scanner did not flag the unredacted input, so the assertion " +
			"above is vacuous")
	}
}
