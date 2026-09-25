package cli

// Terminal rendering of a pairing invite as a QR code (#655), so the joining
// device — Cruciform's scanner, or a phone — reads it straight off the screen
// instead of the invite being copied across.
//
// The encoder is github.com/skip2/go-qrcode: pure Go, no dependencies of its
// own, and the encoder voidbind-go's `voidbind pair-initiate` already uses for
// the same invite. The half-block rendering matches voidbind's too, so the two
// tools print the same code the same way.

import (
	"fmt"
	"io"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// Half-block glyphs. Each character cell shows TWO module rows, top and bottom,
// which keeps the code roughly square in a terminal whose cells are twice as
// tall as they are wide. LIGHT modules are drawn in the foreground colour and
// DARK modules as the background, which gives a correctly polarised code on a
// dark terminal. On a light terminal it comes out inverted, and mainstream
// scanners read that too.
const (
	glyphBothLight   = "█"
	glyphTopLight    = "▀"
	glyphBottomLight = "▄"
	glyphBothDark    = " "
)

// qrModules encodes payload at medium error correction and returns its module
// matrix, quiet zone included (true = dark).
func qrModules(payload string) ([][]bool, error) {
	q, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		return nil, fmt.Errorf("encoding the invite as a QR code: %w", err)
	}
	return q.Bitmap(), nil
}

// renderQR writes payload to w as a half-block QR code.
func renderQR(w io.Writer, payload string) error {
	modules, err := qrModules(payload)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, renderModules(modules))
	return err
}

// renderModules is the pure step: a module matrix (true = dark) to half-block
// text, one line per two module rows. An odd last row is paired with a light
// row, as the quiet zone would be.
func renderModules(modules [][]bool) string {
	dark := func(r, c int) bool {
		return r < len(modules) && c < len(modules[r]) && modules[r][c]
	}
	var sb strings.Builder
	for r := 0; r < len(modules); r += 2 {
		for c := range modules[r] {
			switch top, bottom := dark(r, c), dark(r+1, c); {
			case !top && !bottom:
				sb.WriteString(glyphBothLight)
			case !top && bottom:
				sb.WriteString(glyphTopLight)
			case top && !bottom:
				sb.WriteString(glyphBottomLight)
			default:
				sb.WriteString(glyphBothDark)
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// showQR decides whether to render the QR: --no-qr wins, --qr forces it, and
// otherwise it is shown only when stdout is a terminal. A pipe, a file or a
// script's buffer gets the invite string alone, which is what a script parses.
func showQR(forceOn, forceOff bool, stdout io.Writer) bool {
	switch {
	case forceOff:
		return false
	case forceOn:
		return true
	default:
		return isTerminal(stdout)
	}
}
