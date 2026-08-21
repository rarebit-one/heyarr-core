package fixtures

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
)

// The credential scanner.
//
// # What this is for, and why it is not tidiness
//
// A real capture contains a real API key, real tracker URLs and real announce
// URLs. This repository is public and AGPL, and git history is permanent — so
// a key that reaches a committed fixture is a live credential in a permanent
// public record, and rotating it afterwards does not remove it.
//
// Redaction happens at capture time. This exists to catch the time it does
// not: the next person captures on a machine whose redaction rules are
// whatever they were that afternoon, against a service whose response shape
// nobody anticipated.
//
// # It runs in CI, over the committed corpus
//
// Not only at capture. A guard that runs where the mistake is made protects
// the person who remembered to run it; a guard that runs on every push
// protects everyone else.
//
// # It is deliberately noisy rather than clever
//
// A scanner tuned to miss nothing will flag things that are fine, and the
// answer to that is an explicit allow — a decision, in a diff, that somebody
// made — rather than a looser pattern. A looser pattern is a decision too, but
// it is one nobody sees.

// Finding is one suspicious value.
type Finding struct {
	Path string
	Line int
	// Rule names what matched, so a reader knows whether to redact or to allow.
	Rule string
	// Excerpt is the surrounding text with the suspected secret ELIDED.
	//
	// Printing the match would defeat the purpose: this output goes to CI
	// logs, which are as public as the repository.
	Excerpt string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s — %s", f.Path, f.Line, f.Rule, f.Excerpt)
}

// rule is one pattern and what it is looking for.
type rule struct {
	name string
	re   *regexp.Regexp
}

// rules are what a leaked credential looks like in these corpora.
//
// Aimed at the shapes that actually occur in indexer and download-client
// traffic rather than at a general secret scanner: this runs over a corpus
// whose whole content is API traffic, so a generic "high entropy string" rule
// would flag every infohash in every fixture and be turned off within a day.
var rules = []rule{
	{
		// Prowlarr and the *arr stack use a 32-character lower-case hex API
		// key, passed as ?apikey= or an X-Api-Key header. The redacted form is
		// the literal REDACTED, so anything hex-shaped here is a real one.
		name: "arr-api-key",
		re:   regexp.MustCompile(`(?i)\b(?:api[_-]?key|apikey)\b["'\s:=]+([0-9a-f]{32})`),
	},
	{
		// A Transmission RPC session id, which is not a long-lived credential
		// but is a live token while it lasts.
		name: "transmission-session-id",
		re:   regexp.MustCompile(`(?i)X-Transmission-Session-Id["'\s:=]+([A-Za-z0-9]{20,})`),
	},
	{
		// Basic auth embedded in a URL — the classic way a download client
		// credential ends up in a config and then in a capture.
		name: "credentials-in-url",
		re:   regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/\s"']*:[^/\s"'@]+@`),
	},
	{
		// A tracker announce URL carrying a passkey. This is the one that
		// matters most and is easiest to miss: it identifies a person to a
		// private tracker, and it does not look like a secret.
		//
		// Two shapes, because trackers use both and a rule that knew only the
		// named one would look straight past the other. The bare path segment
		// was missed by the first version of this rule, which is why it is
		// spelled out rather than folded into a cleverer pattern.
		name: "tracker-passkey-named",
		re:   regexp.MustCompile(`(?i)/announce[^"'\s]*[?&](?:passkey|pid|torrent_pass|authkey)=[0-9a-z]{16,}`),
	},
	{
		// .../announce/<long token> — the passkey as a path segment, with
		// nothing naming it.
		name: "tracker-passkey-path",
		re:   regexp.MustCompile(`(?i)/announce/[0-9a-z]{16,}`),
	},
	{
		name: "authorization-header",
		re:   regexp.MustCompile(`(?i)"Authorization"\s*:\s*"(?:Bearer|Basic)\s+[A-Za-z0-9+/=._-]{16,}"`),
	},
	{
		name: "private-key-block",
		re:   regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	},
}

// Redacted is the placeholder a capture writes in place of a secret.
//
// A single literal, so the scanner can recognise a redaction and so a reader
// can tell a redacted value from a value that merely looks odd.
const Redacted = "REDACTED"

// Scan walks a directory and reports anything credential-shaped.
//
// Every file is scanned, not only JSON: a stray .har, .txt or .log dropped
// into the corpus while debugging is exactly the kind of thing that gets
// committed by accident, and it is where a scanner scoped to .json would look
// away.
func Scan(root string) ([]Finding, error) {
	var findings []Finding

	// os.Root rather than filepath.WalkDir, so the walk cannot be walked OUT
	// of by a symlink.
	//
	// This matters more here than it looks. The scanner exists to prove a
	// directory contains no credentials; a symlink inside it pointing at
	// ~/.config would make the scanner read — and report excerpts from — files
	// it was never meant to see. A guard that can be pointed elsewhere by its
	// own input is not a guard.
	dirRoot, err := os.OpenRoot(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No corpus yet is not a failure. It is the normal state until
			// somebody with a real instance captures one, and CI must not go
			// red for it.
			return nil, nil
		}
		return nil, fmt.Errorf("fixtures: opening %s: %w", root, err)
	}
	defer func() { _ = dirRoot.Close() }()

	err = fs.WalkDir(dirRoot.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := dirRoot.Open(path)
		if err != nil {
			return fmt.Errorf("fixtures: reading %s: %w", path, err)
		}
		defer func() { _ = f.Close() }()
		raw, err := io.ReadAll(f)
		if err != nil {
			return fmt.Errorf("fixtures: reading %s: %w", path, err)
		}
		findings = append(findings, scanBytes(path, string(raw))...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

func scanBytes(path, content string) []Finding {
	var out []Finding
	for i, line := range strings.Split(content, "\n") {
		// A line that is entirely a redaction is the thing working. Skipping
		// it keeps the credentials-in-url rule from flagging
		// "http://REDACTED:REDACTED@host", which is a correctly redacted
		// capture and would otherwise be the most common false positive.
		if strings.Contains(line, Redacted) {
			continue
		}
		for _, r := range rules {
			if loc := r.re.FindStringIndex(line); loc != nil {
				out = append(out, Finding{
					Path: path, Line: i + 1, Rule: r.name,
					Excerpt: elide(line, loc[0], loc[1]),
				})
			}
		}
	}
	return out
}

// elide renders the surrounding context with the match itself removed.
//
// The scanner's output goes to CI logs, which are as public as the repository.
// Printing the match would turn the guard into a second way of publishing the
// secret — which is a real failure mode of secret scanners and an easy one to
// walk into while trying to be helpful.
func elide(line string, start, end int) string {
	const context = 24
	from := max(start-context, 0)
	to := min(end+context, len(line))
	return strings.TrimSpace(line[from:start]) + " «ELIDED» " + strings.TrimSpace(line[end:to])
}
