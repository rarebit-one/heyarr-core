package yubikey

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
)

// The backend implements the client.Unwrapper seam (ADR-0098) — the whole point.
var _ client.Unwrapper = (*Unwrapper)(nil)

func TestParsePubkeyPoint(t *testing.T) {
	t.Parallel()
	point := make([]byte, 32)
	for i := range point {
		point[i] = byte(i + 1)
	}
	// A SCD READKEY canonical S-expression with the 33-byte 0x40-prefixed q.
	sexp := []byte("(10:public-key(3:ecc(5:curve10:Curve25519)(5:flags9:djb-tweak)(1:q33:" +
		"\x40" + string(point) + ")))")

	got, err := parsePubkeyPoint(sexp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !bytes.Equal(got, point) {
		t.Fatalf("point = %x, want %x", got, point)
	}

	for _, tc := range []struct {
		name string
		in   string
	}{
		{"no q", "(10:public-key(3:ecc(5:curve10:Curve25519)))"},
		{"truncated", "(1:q33:\x40short"},
		{"wrong prefix", "(1:q33:\x41" + strings.Repeat("x", 32)},
	} {
		if _, err := parsePubkeyPoint([]byte(tc.in)); err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
	}
}

func TestDecipherDO(t *testing.T) {
	t.Parallel()
	eph := make([]byte, 32)
	eph[0], eph[31] = 0xAA, 0xBB
	do, err := decipherDO(eph)
	if err != nil {
		t.Fatal(err)
	}
	// Cipher DO A6 25 { 7F49 22 { 86 20 <32> } } — 39 bytes, 78 hex chars.
	want := "A6257F49228620" + hex.EncodeToString(eph)
	if do != want {
		t.Fatalf("DO = %s, want %s", do, want)
	}
	if len(do)/2 != 39 {
		t.Fatalf("DO length = %d bytes, want 39", len(do)/2)
	}
	// The whole PSO:DECIPHER APDU a caller assembles round-trips the length byte.
	apdu := fmt.Sprintf("002A8086%02X%s00", len(do)/2, do)
	if !strings.HasPrefix(apdu, "002A808627A6257F49228620") {
		t.Fatalf("apdu = %s", apdu)
	}

	for _, n := range []int{0, 31, 33, 64} {
		if _, err := decipherDO(make([]byte, n)); err == nil {
			t.Errorf("ephemeral len %d: want error, got nil", n)
		}
	}
}

func TestUnescape(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"ABC", "ABC"},
		{"%41%42%43", "ABC"},
		{"a%25b", "a%b"},
		{"%0Ax", "\nx"},
		{"trailing%", "trailing%"}, // a bare % at the end is left as-is
	} {
		if got := string(unescape([]byte(tc.in))); got != tc.want {
			t.Errorf("unescape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCheckSW(t *testing.T) {
	t.Parallel()
	body, err := checkSW([]byte{0x01, 0x02, 0x90, 0x00}, "op")
	if err != nil || !bytes.Equal(body, []byte{0x01, 0x02}) {
		t.Fatalf("body = %x, err = %v", body, err)
	}
	for _, tc := range []struct {
		name string
		resp []byte
	}{
		{"pin blocked", []byte{0x69, 0x83}},
		{"cond not satisfied", []byte{0x69, 0x85}},
		{"short", []byte{0x90}},
	} {
		if _, err := checkSW(tc.resp, "op"); err == nil {
			t.Errorf("%s: want error", tc.name)
		}
	}
}

func TestNewRequiresPIN(t *testing.T) {
	t.Parallel()
	if _, err := New("", nil); err == nil {
		t.Fatal("New with nil PINFunc: want error")
	}
}
