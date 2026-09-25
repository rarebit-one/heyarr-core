package cli

// Terminal rendering of a pairing invite as a QR code (#655), so the joining
// device — Cruciform's scanner, or a phone — reads it straight off the screen
// instead of the invite being copied across.
//
// The encoder is github.com/boombuler/barcode/qr: pure Go, no dependencies of
// its own, and it reaches no further into the standard library than `image`
// and `image/color`. That last property is not incidental. §69's render guard
// (internal/controller/render_guard_test.go) forbids an image codec anywhere in
// the import graph, and the encoder voidbind-go uses for the same invite,
// skip2/go-qrcode, imports image/png for its PNG output. The half-block
// rendering matches voidbind's, so the two tools draw the code the same way.

import (
	"fmt"
	"io"
	"strings"

	"github.com/boombuler/barcode/qr"
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

// quietZone is the light margin a scanner needs around the symbol, in modules.
// ISO/IEC 18004 asks for four.
const quietZone = 4

// qrModules encodes payload at medium error correction and returns its module
// matrix, quiet zone included (true = dark).
func qrModules(payload string) ([][]bool, error) {
	code, err := qr.Encode(payload, qr.M, qr.Auto)
	if err != nil {
		return nil, fmt.Errorf("encoding the invite as a QR code: %w", err)
	}
	size := code.Bounds().Dx()
	modules := make([][]bool, size+2*quietZone)
	for row := range modules {
		modules[row] = make([]bool, size+2*quietZone)
	}
	for y := range size {
		for x := range size {
			// The default scheme draws dark modules black and light ones white.
			r, g, b, _ := code.At(x, y).RGBA()
			modules[y+quietZone][x+quietZone] = r+g+b < 3*0x8000
		}
	}
	return modules, nil
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
