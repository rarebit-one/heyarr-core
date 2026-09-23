// Package vaultframe is the vault content codec (ADR-0097): it turns a plaintext
// file into fixed-size, independently-decryptable ciphertext frames plus an
// encrypted manifest, so a client can range-read and decrypt part of a vault file
// without the whole object, and the peer stores only ciphertext it cannot read
// (ADR-0021, ADR-0049).
//
// Each frame seals `header ‖ data` under the space key with the existing
// per-frame AEAD (encryption.EncryptChange), where the 21-byte header —
// version ‖ file_id ‖ frame_index — is authenticated with the data. During a
// range read the whole-blob id is not verified, so this header is what stops a
// peer substituting one frame's ciphertext for another's: a reordered frame fails
// the index check and a frame spliced from another object fails the file_id check.
// The frame COUNT is not in the header (it is unknown during single-pass sealing
// and redundant); it lives in the manifest, which is itself sealed and therefore
// authenticated.
//
// This first implementation lives in heyarr and composes voidbind's per-frame
// primitive so W1 stays single-repo and dependency-free; it is lifted into
// voidbind-go behind this same wire format once it has settled (ADR-0097).
package vaultframe

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/rarebit-one/voidbind-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
)

// Wire constants (ADR-0097). A change to any is a new manifest Version, never a
// reinterpretation of a shipped one.
const (
	// Version is the frame/manifest wire version.
	Version = 1
	// FrameSize is the fixed plaintext data payload of a full frame (the last
	// frame of a file may be shorter). 1 MiB.
	FrameSize = 1 << 20

	fileIDLen = 16
	// headerLen is version(1) + file_id(16) + frame_index(uint32 BE, 4).
	headerLen = 1 + fileIDLen + 4

	// nonceLen and tagLen mirror voidbind's XChaCha20-Poly1305 framing (a 24-byte
	// nonce prefix and a 16-byte tag; ADR-0049). They are asserted against a real
	// sealed frame in the tests, so a change to the underlying cipher is caught.
	nonceLen = 24
	tagLen   = 16
)

// ErrFrame is a frame that decrypted but did not match its manifest — a wrong
// index, a wrong file id, or a wrong version. It is deliberately one error, like
// encryption.ErrUnwrap, so a caller (and an attacker) learns only "this is not the
// frame you asked for," not which check failed.
var ErrFrame = errors.New("vaultframe: frame does not match its manifest")

// Manifest is a vault object's geometry. It is sealed under the space key
// (SealManifest) and stored as its own ciphertext blob; a drive entry (ADR-0095)
// references that blob. It records the content blob's id so a reader can fetch it.
type Manifest struct {
	Version       int    `json:"version"`
	FileID        string `json:"file_id"` // hex of the 16 random per-object bytes
	FrameSize     int    `json:"frame_size"`
	FrameCount    int    `json:"frame_count"`
	PlaintextSize int64  `json:"plaintext_size"`
	Content       string `json:"content"` // "blake3:<hex>" of the ciphertext content blob
}

func frameHeader(fileID []byte, index uint32) []byte {
	h := make([]byte, headerLen)
	h[0] = Version
	copy(h[1:1+fileIDLen], fileID)
	binary.BigEndian.PutUint32(h[1+fileIDLen:], index)
	return h
}

// Seal reads plaintext from r, seals it into fixed frames under sk, writes the
// concatenated ciphertext to w, and returns the manifest describing it — including
// the content blob id, the BLAKE3 of the ciphertext (never the plaintext, ADR-0021),
// computed as the frames are written. It streams: only one frame is held at a time.
func Seal(sk encryption.SpaceKey, r io.Reader, w io.Writer) (Manifest, error) {
	fileID := make([]byte, fileIDLen)
	if _, err := rand.Read(fileID); err != nil {
		return Manifest{}, fmt.Errorf("vaultframe: drawing a file id: %w", err)
	}
	h := hashing.New()
	mw := io.MultiWriter(w, h)

	buf := make([]byte, FrameSize)
	var total int64
	index := 0
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if index > math.MaxUint32 {
				return Manifest{}, fmt.Errorf("vaultframe: file has more than %d frames", math.MaxUint32)
			}
			plaintext := append(frameHeader(fileID, uint32(index)), buf[:n]...)
			sealed, serr := encryption.EncryptChange(sk, plaintext)
			if serr != nil {
				return Manifest{}, fmt.Errorf("vaultframe: sealing frame %d: %w", index, serr)
			}
			if _, werr := mw.Write(sealed); werr != nil {
				return Manifest{}, fmt.Errorf("vaultframe: writing frame %d: %w", index, werr)
			}
			total += int64(n)
			index++
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("vaultframe: reading plaintext: %w", err)
		}
	}
	return Manifest{
		Version:       Version,
		FileID:        hex.EncodeToString(fileID),
		FrameSize:     FrameSize,
		FrameCount:    index,
		PlaintextSize: total,
		Content:       h.Sum().String(),
	}, nil
}

// fullFrameCipherLen is the ciphertext length of a full frame of this manifest.
func (m Manifest) fullFrameCipherLen() int64 {
	return int64(nonceLen+headerLen) + int64(m.FrameSize) + int64(tagLen)
}

// FrameByteRange returns the [start, end) byte range within the content blob that
// holds frame index, so a client can range-fetch exactly that frame. The last
// frame may be short; its length follows from the plaintext total.
func (m Manifest) FrameByteRange(index int) (start, end int64) {
	L := m.fullFrameCipherLen()
	start = int64(index) * L
	if index == m.FrameCount-1 {
		lastData := m.PlaintextSize - int64(index)*int64(m.FrameSize)
		end = start + int64(nonceLen+headerLen) + lastData + int64(tagLen)
	} else {
		end = start + L
	}
	return start, end
}

// OpenFrame decrypts one frame from its sealed bytes and verifies its header
// against this manifest (this object's file id, this index). It returns the
// frame's plaintext data, or ErrFrame if the frame is not the one expected.
func (m Manifest) OpenFrame(sk encryption.SpaceKey, index int, sealed []byte) ([]byte, error) {
	plaintext, err := encryption.DecryptChange(sk, sealed)
	if err != nil {
		return nil, err
	}
	if len(plaintext) < headerLen || plaintext[0] != byte(Version) {
		return nil, ErrFrame
	}
	wantID, err := hex.DecodeString(m.FileID)
	if err != nil {
		return nil, fmt.Errorf("vaultframe: manifest file id: %w", err)
	}
	if !bytes.Equal(plaintext[1:1+fileIDLen], wantID) {
		return nil, ErrFrame
	}
	if int(binary.BigEndian.Uint32(plaintext[1+fileIDLen:headerLen])) != index {
		return nil, ErrFrame
	}
	return plaintext[headerLen:], nil
}

// OpenRange returns plaintext[off : off+n], fetching only the frames that cover
// it. fetch(start, end) returns the ciphertext bytes [start, end) of the content
// blob — a client backs it with a ranged GET of the blob. It over-fetches whole
// frames and trims after decrypt (ADR-0021).
func (m Manifest) OpenRange(sk encryption.SpaceKey, off, n int64, fetch func(start, end int64) ([]byte, error)) ([]byte, error) {
	if off < 0 || n < 0 || off+n > m.PlaintextSize {
		return nil, fmt.Errorf("vaultframe: range [%d,%d) is outside the %d-byte file", off, off+n, m.PlaintextSize)
	}
	if n == 0 {
		return []byte{}, nil
	}
	first := int(off / int64(m.FrameSize))
	last := int((off + n - 1) / int64(m.FrameSize))
	out := make([]byte, 0, int64(last-first+1)*int64(m.FrameSize))
	for i := first; i <= last; i++ {
		start, end := m.FrameByteRange(i)
		fc, err := fetch(start, end)
		if err != nil {
			return nil, err
		}
		data, err := m.OpenFrame(sk, i, fc)
		if err != nil {
			return nil, err
		}
		out = append(out, data...)
	}
	lo := off - int64(first)*int64(m.FrameSize)
	return out[lo : lo+n], nil
}

// SealManifest seals a manifest under the space key, returning the ciphertext blob
// a peer stores and a drive entry references.
func SealManifest(sk encryption.SpaceKey, m Manifest) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("vaultframe: encoding manifest: %w", err)
	}
	return encryption.EncryptChange(sk, b)
}

// OpenManifest reverses SealManifest.
func OpenManifest(sk encryption.SpaceKey, sealed []byte) (Manifest, error) {
	b, err := encryption.DecryptChange(sk, sealed)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("vaultframe: decoding manifest: %w", err)
	}
	return m, nil
}
