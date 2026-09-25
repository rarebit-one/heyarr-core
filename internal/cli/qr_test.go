package cli

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// A fixed invite of the real shape, so the golden file is a picture of what an
// operator sees rather than of a random session.
const sampleInvite = "voidbind:pair?relay=http%3A%2F%2F127.0.0.1%3A8420%2Fpair&session=" +
	"0123456789abcdef0123456789abcdef&salt=00112233445566778899aabbccddeeff" +
	"&user=ed25519%3A" + "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"

// parseRendered inverts renderModules: the four glyphs must be a bijection with
// the four (top, bottom) module pairs, or a scanner reads a different code.
func parseRendered(text string) [][]bool {
	var out [][]bool
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		var top, bottom []bool
		for _, ch := range line {
			switch string(ch) {
			case glyphBothLight:
				top, bottom = append(top, false), append(bottom, false)
			case glyphTopLight:
				top, bottom = append(top, false), append(bottom, true)
			case glyphBottomLight:
				top, bottom = append(top, true), append(bottom, false)
			default:
				top, bottom = append(top, true), append(bottom, true)
			}
		}
		out = append(out, top, bottom)
	}
	return out
}

// TestRenderedQRIsTheEncodedInvite: the text drawn on screen carries exactly the
// modules the encoder produced for the invite, for payloads of several sizes.
// Every QR symbol has an odd number of rows, so the padded last line is
// exercised by every case.
func TestRenderedQRIsTheEncodedInvite(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{"x", sampleInvite, strings.Repeat(sampleInvite, 3)} {
		modules, err := qrModules(payload)
		if err != nil {
			t.Fatal(err)
		}
		if len(modules)%2 == 0 {
			t.Fatalf("a QR symbol with %d rows; the padding case is not being exercised", len(modules))
		}
		var buf bytes.Buffer
		if err := renderQR(&buf, payload); err != nil {
			t.Fatal(err)
		}
		got := parseRendered(buf.String())
		// The padded row renders as light, which is what the quiet zone is.
		want := append(modules, make([]bool, len(modules[0])))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("payload of %d bytes: the rendered code does not decode to the encoded modules", len(payload))
		}
	}
}

// TestRenderedQRGolden pins the picture itself, so a change to the glyphs, the
// polarity or the error-correction level shows up as a diff to review.
func TestRenderedQRGolden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderQR(&buf, sampleInvite); err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, "testdata/pair_invite_qr.txt", buf.Bytes())
}

func TestShowQRDecision(t *testing.T) {
	t.Parallel()
	var pipe bytes.Buffer
	for _, tc := range []struct {
		name         string
		forceOn, off bool
		want         bool
	}{
		{"not a terminal, no flag", false, false, false},
		{"--qr", true, false, true},
		{"--no-qr", false, true, false},
	} {
		if got := showQR(tc.forceOn, tc.off, &pipe); got != tc.want {
			t.Errorf("%s: showQR = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The default-on case needs a character device standing in for a terminal.
	if runtime.GOOS == "windows" {
		return
	}
	tty, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()
	if !showQR(false, false, tty) {
		t.Error("a terminal with no flag: showQR = false, want true")
	}
	if showQR(false, true, tty) {
		t.Error("a terminal with --no-qr: showQR = true, want false")
	}
}

// TestPairAuthoriseDrawsTheInviteAsAQR runs the real command tree: with --qr the
// output carries a code that decodes to the very invite printed above it, the
// invite line is still there for scripts, and the pairing completes. Without a
// flag, into a buffer, no code is drawn at all.
func TestPairAuthoriseDrawsTheInviteAsAQR(t *testing.T) {
	relayAddr := relayServer(t)
	idDir := identityDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, _, err := run(t, ctx, "identity", "generate", "--identity-dir", idDir, "--name", "owner"); err != nil {
		t.Fatalf("identity generate: %v", err)
	}

	for _, tc := range []struct {
		name string
		flag []string
		want bool
	}{
		{"--qr", []string{"--qr"}, true},
		{"default into a pipe", nil, false},
		{"--no-qr", []string{"--no-qr"}, false},
	} {
		auth := append([]string{
			"--identity-dir", idDir, "--device-dir", deviceDir(t), "--relay", relayAddr,
			"--yes", "--poll", "10ms",
		}, tc.flag...)
		res := runPair(t, ctx, auth, []string{"--device-dir", deviceDir(t), "--yes", "--poll", "10ms"})
		assertPaired(t, res)

		out := res.authOut.String()
		invite := extractLine(out, "invite:")
		drawn := qrBlock(out)
		if (drawn != "") != tc.want {
			t.Fatalf("%s: QR drawn = %v, want %v:\n%s", tc.name, drawn != "", tc.want, out)
		}
		if !tc.want {
			continue
		}
		modules, err := qrModules(invite)
		if err != nil {
			t.Fatal(err)
		}
		want := append(modules, make([]bool, len(modules[0])))
		if !reflect.DeepEqual(parseRendered(drawn), want) {
			t.Fatalf("%s: the drawn code is not the printed invite", tc.name)
		}
	}
}

func TestPairAuthoriseRefusesBothQRFlags(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := run(t, ctx, "pair", "authorise", "--relay", "127.0.0.1:1", "--qr", "--no-qr")
	if err == nil || !strings.Contains(err.Error(), "none of the others can be") {
		t.Fatalf("want a mutually-exclusive flag error, got %v", err)
	}
}

// qrBlock returns the lines of out drawn in half-block glyphs, or "".
func qrBlock(out string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if strings.ContainsAny(line, glyphBothLight+glyphTopLight+glyphBottomLight) {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}
