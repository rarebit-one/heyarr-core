package recovery

import (
	"errors"
	"fmt"
	"strings"
)

// This file is a self-contained bech32m codec (BIP-350), the checksummed
// human-transcribable encoding the recovery secret is displayed in.
//
// Why bech32m rather than a bare base32 or a hand-rolled check digit: it is a
// reviewed specification with a stated, provable error-detection property. Its
// checksum is a BCH code over GF(32) that GUARANTEES detection of any error
// touching at most four characters, and detects a longer corruption with
// probability better than 1 - 2^-30. That is the "fails LOUD" mechanism
// ADR-0022 demands of the base secret — the SLIP-39-over-plain-Shamir concern
// ("a corrupted share does not fail loudly") applied one level down. A single
// transcription slip does not derive a different key; it fails to decode.
//
// bech32m is used rather than the original bech32 (BIP-173) because bech32's
// checksum has a known weakness — inserting or deleting a final "q" is not
// always detected — that BIP-350 fixes by changing the constant below. Both are
// reviewed; the newer one has the cleaner guarantee, and nothing here needs
// segwit's bech32-for-v0 compatibility.
//
// The charset deliberately omits the ambiguous glyphs 1, b, i and o, so a
// hand-copied secret has fewer ways to go wrong in the first place — and the
// checksum catches the rest.

// charset is the bech32 alphabet (BIP-173): 32 symbols, no 1/b/i/o.
const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// bech32mConst is the BIP-350 checksum constant. The original bech32 used 1;
// this value is what gives bech32m its clean four-character guarantee.
const bech32mConst = 0x2bc830a3

// ErrBech32 is a structurally invalid encoding: no separator, an out-of-charset
// character, mixed case, or a checksum that does not verify. It is deliberately
// one error for every "this is not a valid, intact secret" case — the caller
// that matters (ParseSecret) wraps it into the package's public sentinels.
var ErrBech32 = errors.New("recovery: not a valid bech32m string")

// revCharset maps a symbol back to its 5-bit value, or -1. Built once.
var revCharset = func() [128]int8 {
	var t [128]int8
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(charset); i++ {
		t[charset[i]] = int8(i)
	}
	return t
}()

// polymod is the bech32 BCH residue over GF(32). It is the whole error-detection
// engine: createChecksum picks six symbols that drive this to bech32mConst, and
// verify checks that it still lands there.
func polymod(values []int) int {
	gen := [5]int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := 1
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ v
		for i := 0; i < 5; i++ {
			if (top>>i)&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

// hrpExpand spreads the human-readable prefix into the high bits, a zero, then
// the low bits — so the checksum is bound to the prefix and a secret decoded
// under the wrong prefix fails.
func hrpExpand(hrp string) []int {
	out := make([]int, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, int(hrp[i])>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, int(hrp[i])&31)
	}
	return out
}

// createChecksum returns the six checksum symbols for hrp and 5-bit data.
func createChecksum(hrp string, data []int) []int {
	values := append(hrpExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := polymod(values) ^ bech32mConst
	out := make([]int, 6)
	for i := 0; i < 6; i++ {
		out[i] = (mod >> uint(5*(5-i))) & 31
	}
	return out
}

// verifyChecksum reports whether hrp and 5-bit data (checksum included) verify.
func verifyChecksum(hrp string, data []int) bool {
	return polymod(append(hrpExpand(hrp), data...)) == bech32mConst
}

// bech32mEncode renders hrp and 5-bit data as "<hrp>1<data><checksum>".
func bech32mEncode(hrp string, data []int) string {
	sum := createChecksum(hrp, data)
	var b strings.Builder
	b.Grow(len(hrp) + 1 + len(data) + len(sum))
	b.WriteString(hrp)
	b.WriteByte('1')
	for _, d := range append(append([]int{}, data...), sum...) {
		b.WriteByte(charset[d])
	}
	return b.String()
}

// bech32mSplit does the STRUCTURAL half of decoding: it validates case, the
// separator and the alphabet, and returns the prefix and the 5-bit symbols
// INCLUDING the six checksum symbols. It does not verify the checksum — that is
// the caller's separate [verifyChecksum] step, kept apart so a well-formed
// string with a wrong checksum (a transcription slip) can be reported
// differently from a structurally invalid one.
func bech32mSplit(s string) (string, []int, error) {
	if s != strings.ToLower(s) && s != strings.ToUpper(s) {
		return "", nil, fmt.Errorf("%w: mixed case", ErrBech32)
	}
	s = strings.ToLower(s)
	pos := strings.LastIndexByte(s, '1')
	if pos < 1 || pos+7 > len(s) {
		// No separator, an empty prefix, or too few characters after it to hold
		// the six-symbol checksum.
		return "", nil, fmt.Errorf("%w: missing separator or checksum", ErrBech32)
	}
	hrp := s[:pos]
	dataPart := s[pos+1:]
	symbols := make([]int, 0, len(dataPart))
	for i := 0; i < len(dataPart); i++ {
		c := dataPart[i]
		if c >= 128 || revCharset[c] < 0 {
			return "", nil, fmt.Errorf("%w: %q is not in the alphabet", ErrBech32, string(c))
		}
		symbols = append(symbols, int(revCharset[c]))
	}
	return hrp, symbols, nil
}

// convertBits regroups a byte/5-bit stream between bases. Encoding uses 8→5 with
// padding; decoding uses 5→8 without, and rejects a value that leaves non-zero
// padding bits — the last defence against a trailing-garbage encoding.
func convertBits(data []int, fromBits, toBits uint, pad bool) ([]int, error) {
	var acc, bits uint
	maxv := (1 << toBits) - 1
	out := make([]int, 0, len(data)*int(fromBits)/int(toBits)+1)
	for _, value := range data {
		if value < 0 || value>>fromBits != 0 {
			return nil, fmt.Errorf("%w: value out of range", ErrBech32)
		}
		acc = acc<<fromBits | uint(value)
		bits += fromBits
		for bits >= toBits {
			bits -= toBits
			out = append(out, int(acc>>bits)&maxv)
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, int(acc<<(toBits-bits))&maxv)
		}
	} else if bits >= fromBits || (acc<<(toBits-bits))&uint(maxv) != 0 {
		return nil, fmt.Errorf("%w: non-canonical padding", ErrBech32)
	}
	return out, nil
}

// bytesToBase32 packs bytes into 5-bit groups for encoding.
func bytesToBase32(b []byte) ([]int, error) {
	in := make([]int, len(b))
	for i, x := range b {
		in[i] = int(x)
	}
	return convertBits(in, 8, 5, true)
}

// base32ToBytes unpacks 5-bit groups back into bytes.
func base32ToBytes(data []int) ([]byte, error) {
	out, err := convertBits(data, 5, 8, false)
	if err != nil {
		return nil, err
	}
	b := make([]byte, len(out))
	for i, x := range out {
		// convertBits masks every 8-bit output to 0..255, so this cannot
		// truncate; the mask restates that for the reader and the linter.
		b[i] = byte(x & 0xff)
	}
	return b, nil
}
