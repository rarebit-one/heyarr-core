package fixtures

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scanner's teeth are tested before its silence is trusted.
//
// A guard nobody has watched catch anything is decoration — and this one
// guards a permanent, public mistake, so "it has never fired" is not evidence
// that it works.

func TestTheScannerCatchesEachShapeOfLeak(t *testing.T) {
	cases := []struct {
		name    string
		content string
		rule    string
	}{
		{
			name:    "a Prowlarr API key in a query string",
			content: `{"path":"/api/v1/search?apikey=0123456789abcdef0123456789abcdef&query=x"}`,
			rule:    "arr-api-key",
		},
		{
			name:    "a Prowlarr API key in a header",
			content: `{"headers":{"X-Api-Key":"fedcba9876543210fedcba9876543210"}}`,
			rule:    "arr-api-key",
		},
		{
			name:    "a Transmission session id",
			content: `{"X-Transmission-Session-Id":"AbCd1234EfGh5678IjKl9012MnOp"}`,
			rule:    "transmission-session-id",
		},
		{
			name:    "credentials embedded in a URL",
			content: `{"endpoint":"http://someone:hunter2@downloads.invalid:9091/transmission/rpc"}`,
			rule:    "credentials-in-url",
		},
		{
			// The one that matters most and looks least like a secret: it
			// identifies a person to a private tracker.
			name:    "a tracker passkey in an announce URL",
			content: `{"announce":"https://tracker.invalid/announce?passkey=0123456789abcdef0123456789"}`,
			rule:    "tracker-passkey-named",
		},
		{
			// The shape the first version of this rule looked straight past:
			// a passkey as a bare path segment, with nothing naming it.
			name:    "a passkey as a path segment",
			content: `{"announce":"https://tracker.invalid/announce/0123456789abcdef0123456789"}`,
			rule:    "tracker-passkey-path",
		},
		{
			name:    "an Authorization header",
			content: `{"headers":{"Authorization":"Bearer abcdefghijklmnopqrstuvwxyz012345"}}`,
			rule:    "authorization-header",
		},
		{
			name:    "a private key that wandered in",
			content: "-----BEGIN OPENSSH PRIVATE KEY-----\nnot really\n",
			rule:    "private-key-block",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "x.json"), tc.content)

			found, err := Scan(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(found) == 0 {
				t.Fatalf("the scanner missed it entirely")
			}
			var matched bool
			for _, f := range found {
				if f.Rule == tc.rule {
					matched = true
				}
			}
			if !matched {
				t.Errorf("caught by %v, expected the %s rule", found, tc.rule)
			}
		})
	}
}

// The scanner's output goes to CI logs, which are as public as the repository.
// A guard that prints the secret it found is a second way of publishing it —
// a real failure mode, and an easy one to walk into while trying to be helpful.
func TestAFindingNeverPrintsTheSecret(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	dir := t.TempDir()
	write(t, filepath.Join(dir, "x.json"),
		`{"path":"/api/v1/search?apikey=`+secret+`&query=arrival"}`)

	found, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("expected one finding, got %d", len(found))
	}
	rendered := found[0].String()
	if strings.Contains(rendered, secret) {
		t.Fatalf("the finding printed the secret it found:\n%s", rendered)
	}
	// It still has to be actionable: the path, the line and the rule are what
	// tell someone where to look.
	for _, want := range []string{"x.json", "arr-api-key", "ELIDED"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("a finding must still locate the problem; %q missing from:\n%s",
				want, rendered)
		}
	}
}

// A correctly redacted capture must be silent, or the guard gets turned off.
func TestARedactedCaptureIsClean(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "search.json"), `{
		"path": "/api/v1/search?apikey=`+Redacted+`&query=arrival",
		"headers": {"X-Api-Key": "`+Redacted+`", "Authorization": "Bearer `+Redacted+`"},
		"endpoint": "http://`+Redacted+`:`+Redacted+`@downloads.invalid:9091/transmission/rpc",
		"announce": "https://tracker.invalid/announce?passkey=`+Redacted+`"
	}`)

	found, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("a redacted capture must be clean, got %v", found)
	}
}

// An infohash is not a secret, and a scanner that flags one will be turned off
// within a day — every torrent fixture is full of them.
func TestOrdinaryCorpusContentIsNotFlagged(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "torrent.json"), `{
		"hashString": "2b3c4d5e6f708192a3b4c5d6e7f80912a3b4c5d6",
		"name": "Some.Release.2160p.mkv",
		"downloadDir": "/data/torrents/movies",
		"blob": "blake3:1111111111111111111111111111111111111111111111111111111111111111",
		"magnet": "magnet:?xt=urn:btih:2b3c4d5e6f708192a3b4c5d6e7f80912a3b4c5d6&dn=Some.Release"
	}`)

	found, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("infohashes and blob digests are not credentials, got %v", found)
	}
}

// Every file, not only .json. A stray .har or .log dropped in while debugging
// is exactly what gets committed by accident, and it is where a scanner scoped
// to one extension looks away.
func TestTheScannerReadsEveryFileNotOnlyJSON(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "session.har"),
		`"url": "http://x.invalid/api?apikey=0123456789abcdef0123456789abcdef"`)
	write(t, filepath.Join(dir, "notes.txt"),
		`remember the key is 0123456789abcdef0123456789abcdef`)

	found, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The .har definitely; the .txt only if the shape matched, which here it
	// does not — "the key is" is not "apikey=". One finding is correct and the
	// assertion says which file.
	if len(found) == 0 {
		t.Fatal("a .har file with a key in it was not scanned")
	}
	if found[0].Path != "session.har" {
		t.Errorf("found in %s, expected session.har", found[0].Path)
	}
}

// Nested directories, because the corpus is per-service subdirectories.
func TestTheScannerRecurses(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "prowlarr", "deep")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(nested, "x.json"),
		`{"apikey":"0123456789abcdef0123456789abcdef"}`)

	found, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("expected one finding in a nested directory, got %d", len(found))
	}
	if !strings.HasPrefix(found[0].Path, "prowlarr") {
		t.Errorf("the path should be relative to the corpus root, got %q", found[0].Path)
	}
}

// Scanning an empty corpus is clean rather than an error — the corpus is
// empty until somebody with a real instance captures one, and CI must not go
// red for that.
func TestAnEmptyCorpusIsClean(t *testing.T) {
	found, err := Scan(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("an empty corpus produced findings: %v", found)
	}
}

// THE assertion this whole package exists for, and the one #100 calls
// not-optional: a plausible key planted in the committed corpus makes CI go
// red. It runs against the REAL corpus directory, not a temporary one.
func TestTheCommittedCorpusIsClean(t *testing.T) {
	found, err := Scan(corpusDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("the committed fixture corpus contains %d credential-shaped value(s):\n%s",
			len(found), render(found))
	}
}

func render(fs []Finding) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = "  " + f.String()
	}
	return strings.Join(parts, "\n")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The scanner exists to prove a directory contains no credentials. A symlink
// inside it pointing at somewhere else would make it read — and report
// excerpts from — files it was never meant to see.
//
// A guard that can be pointed elsewhere by its own input is not a guard, which
// is why the walk is os.Root-scoped rather than filepath.WalkDir.
func TestTheScannerCannotBeWalkedOutOfItsRoot(t *testing.T) {
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secrets.json"),
		`{"apikey":"0123456789abcdef0123456789abcdef"}`)

	corpus := t.TempDir()
	write(t, filepath.Join(corpus, "real.json"), `{"name":"fine"}`)
	if err := os.Symlink(outside, filepath.Join(corpus, "escape")); err != nil {
		t.Skipf("this filesystem will not make a symlink: %v", err)
	}

	// It REFUSES rather than skipping, and that is the stronger answer.
	// Silently stepping over the symlink would mean a corpus containing one
	// gets partially scanned and reports clean — which is the failure a
	// scanner must never have.
	found, err := Scan(corpus)
	if err == nil {
		for _, f := range found {
			if strings.HasPrefix(f.Path, "escape") {
				t.Fatalf("the scanner followed a symlink out of its root and read %s", f.Path)
			}
		}
		t.Fatal("a symlink out of the corpus must be refused, not silently skipped — " +
			"a partially scanned corpus that reports clean is the one failure a " +
			"scanner cannot have")
	}
	if !strings.Contains(err.Error(), "escape") {
		t.Errorf("the refusal should name the offending entry, said: %v", err)
	}

	// And the control: pointed AT that directory, it does find the key — so
	// the assertion above is about containment and not about a scanner that
	// stopped working.
	direct, err := Scan(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(direct) == 0 {
		t.Fatal("the planted key was not found even when scanned directly, so the " +
			"containment assertion proves nothing")
	}
}

// Scanning a directory that does not exist is clean rather than an error: the
// corpus is absent until somebody captures one, and CI must not go red for it.
func TestScanningAnAbsentDirectoryIsClean(t *testing.T) {
	found, err := Scan(filepath.Join(t.TempDir(), "not-here"))
	if err != nil {
		t.Fatalf("an absent corpus is the normal state, not an error: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("findings from a directory that does not exist: %v", found)
	}
}
