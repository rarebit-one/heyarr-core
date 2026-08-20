package fixtures

import (
	"encoding/binary"
	"math"
)

// The media builders below produce **structurally valid containers that no
// decoder has ever opened**, and that distinction is deliberate.
//
// Milestone 1 decodes nothing: it hashes bytes, parses paths, and serves byte
// ranges. What it needs from a fixture is a real extension, a plausible header
// and enough bytes to range over. Building a fully decodable MP4 by hand — a
// complete moov with a sample table — and shipping it *unverified*, on a
// machine with no ffprobe anywhere to check it against, would buy false
// confidence rather than coverage: nobody would find out it was wrong until
// Milestone 2 tried to probe it, by which point it looks like a probing bug.
//
// So each builder is parsed back and asserted in media_test.go, and each says
// in its own comment exactly how far its validity goes. Milestone 2 introduces
// remote probing (§29) and must replace the audio and video fixtures with real
// samples at that point.

// box writes an ISO base media file format box: a big-endian size covering the
// header, a four-character type, then the payload (ISO/IEC 14496-12 §4.2).
func box(kind string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(payload)+8)) // #nosec G115 -- fixtures are kilobytes
	copy(out[4:8], kind)
	return append(out, payload...)
}

// MP4 builds an ISO base media file format container: an `ftyp` brand
// declaration, a `free` box, and an `mdat` carrying payload.
//
// The box structure is real and round-trips, and the brands are ones a parser
// recognises. There is no `moov`, so there is no track, no codec and nothing to
// decode. See the note at the top of this file.
func MP4(payload []byte) []byte {
	ftyp := make([]byte, 0, 24)
	ftyp = append(ftyp, "isom"...)                  // major brand
	ftyp = binary.BigEndian.AppendUint32(ftyp, 512) // minor version
	ftyp = append(ftyp, "isomiso2avc1mp41"...)      // compatible brands

	out := box("ftyp", ftyp)
	out = append(out, box("free", []byte("heyarr fixture - not decodable, see media.go"))...)
	return append(out, box("mdat", payload)...)
}

// mpegBitrates maps a bitrate index to MPEG-1 Layer III kbit/s (ISO/IEC
// 11172-3 table 4). Index 0 means "free" and 15 is reserved; neither is used.
var mpegBitrates = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}

// MPEG-1 Layer III constants for the frames MP3 emits.
const (
	mp3BitrateIndex = 9     // 128 kbit/s
	mp3SampleRate   = 44100 // sample-rate index 0
	mp3Samples      = 1152  // samples per frame
)

// MP3FrameLength is the byte length of the frames MP3 produces. Exported so a
// caller can size a fixture in frames and know what it will cost.
func MP3FrameLength() int {
	// ISO/IEC 11172-3: frame length = samples/8 * bitrate / sample rate, plus
	// a padding byte this builder never asks for.
	return mp3Samples / 8 * (mpegBitrates[mp3BitrateIndex] * 1000) / mp3SampleRate
}

// MP3 builds a run of MPEG-1 Layer III frames at 128 kbit/s, 44.1 kHz, joint
// stereo.
//
// The frame headers are genuine — sync word, version, layer, bitrate index and
// sample-rate index are all real values, and the frame length is derived from
// them, so a parser walking frame to frame lands exactly on each sync word.
// media_test.go asserts precisely that. The bytes inside each frame are zero
// rather than a Huffman-coded granule, so there is nothing to decode. See the
// note at the top of this file.
func MP3(frames int) []byte {
	frameLen := MP3FrameLength()
	header := [4]byte{
		0xFF,                       // sync
		0xFB,                       // sync, MPEG-1, Layer III, no CRC
		byte(mp3BitrateIndex << 4), // bitrate index, sample-rate index 0, unpadded
		0x40,                       // joint stereo
	}

	out := make([]byte, 0, frames*frameLen)
	for range frames {
		frame := make([]byte, frameLen)
		copy(frame, header[:])
		out = append(out, frame...)
	}
	return out
}

// FLAC builds a FLAC stream: the `fLaC` marker, then a STREAMINFO metadata
// block carrying a real sample rate, channel count and bit depth.
//
// STREAMINFO is genuine and parses. What follows it is padding rather than
// encoded subframes. See the note at the top of this file.
func FLAC(payload []byte) []byte {
	const (
		blockSize  = 4096
		sampleRate = 44100
		channels   = 2
		bitsPer    = 16
	)

	info := make([]byte, 34)
	binary.BigEndian.PutUint16(info[0:2], blockSize) // minimum block size
	binary.BigEndian.PutUint16(info[2:4], blockSize) // maximum block size
	// info[4:10] is min/max frame size; all-zero is defined as "unknown".

	// 64 packed bits: 20 sample rate, 3 (channels-1), 5 (bits per sample - 1),
	// 36 total samples. Zero total samples is defined as "unknown".
	var packed uint64
	packed |= uint64(sampleRate) << 44
	packed |= uint64(channels-1) << 41
	packed |= uint64(bitsPer-1) << 36
	binary.BigEndian.PutUint64(info[10:18], packed)
	// info[18:34] is the MD5 of the unencoded audio; all-zero means "not
	// computed", which the format explicitly permits.

	out := []byte("fLaC")
	// Metadata block header: last-block flag (0x80) | type 0 (STREAMINFO),
	// then a 24-bit big-endian length.
	out = append(out, 0x80, 0x00, 0x00, 34)
	out = append(out, info...)
	return append(out, payload...)
}

// Matroska builds an EBML document with a Matroska DocType header, followed by
// a Segment element carrying payload.
//
// The EBML header is real: the magic, the element ids and the variable-length
// integers are all encoded correctly, and a parser reads DocType "matroska"
// out of it. The Segment holds opaque bytes rather than tracks and clusters.
// See the note at the top of this file.
func Matroska(payload []byte) []byte {
	// EBML variable-size integers, one-byte form: the marker bit is 0x80, so
	// values up to 127 encode directly. Every length here is small enough.
	ebmlUint := func(id []byte, value uint64) []byte {
		var v []byte
		if value == 0 {
			v = []byte{0}
		}
		for value > 0 {
			v = append([]byte{byte(value & 0xFF)}, v...)
			value >>= 8
		}
		out := append([]byte{}, id...)
		out = append(out, byte(0x80|len(v))) // #nosec G115 -- v is at most 8 bytes
		return append(out, v...)
	}
	ebmlString := func(id []byte, s string) []byte {
		out := append([]byte{}, id...)
		out = append(out, byte(0x80|len(s))) // #nosec G115 -- callers pass short strings
		return append(out, s...)
	}

	var header []byte
	header = append(header, ebmlUint([]byte{0x42, 0x86}, 1)...)            // EBMLVersion
	header = append(header, ebmlUint([]byte{0x42, 0xF7}, 1)...)            // EBMLReadVersion
	header = append(header, ebmlUint([]byte{0x42, 0xF2}, 4)...)            // EBMLMaxIDLength
	header = append(header, ebmlUint([]byte{0x42, 0xF3}, 8)...)            // EBMLMaxSizeLength
	header = append(header, ebmlString([]byte{0x42, 0x82}, "matroska")...) // DocType
	header = append(header, ebmlUint([]byte{0x42, 0x87}, 4)...)            // DocTypeVersion
	header = append(header, ebmlUint([]byte{0x42, 0x85}, 2)...)            // DocTypeReadVersion

	if len(header) > 127 {
		// The one-byte EBML length form tops out at 127. Every header this
		// builder produces is far below that, so exceeding it means the builder
		// changed and the encoding must change with it.
		panic("fixtures: EBML header outgrew the one-byte length form")
	}
	out := []byte{0x1A, 0x45, 0xDF, 0xA3}     // EBML magic
	out = append(out, byte(0x80|len(header))) // #nosec G115 -- bounded above
	out = append(out, header...)

	// Segment (id 0x18538067) with an unknown-length marker, which Matroska
	// permits for streaming, followed by the payload.
	out = append(out, 0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	return append(out, payload...)
}

// JPEG builds a baseline JPEG header: SOI, a real JFIF APP0 segment, an
// optional comment, then EOI.
//
// There is no scan data, so there is no image. It carries the extension, the
// magic bytes and enough structure for the artwork asset role. See the note at
// the top of this file.
func JPEG(comment string) []byte {
	out := []byte{0xFF, 0xD8} // SOI

	app0 := []byte{
		'J', 'F', 'I', 'F', 0x00,
		0x01, 0x02, // version 1.02
		0x00,       // density units: none
		0x00, 0x01, // x density
		0x00, 0x01, // y density
		0x00, 0x00, // no embedded thumbnail
	}
	out = append(out, 0xFF, 0xE0)
	out = binary.BigEndian.AppendUint16(out, uint16(len(app0)+2)) // #nosec G115 -- fixed length
	out = append(out, app0...)

	if comment != "" {
		c := []byte(comment)
		if len(c) > math.MaxUint16-2 {
			c = c[:math.MaxUint16-2]
		}
		out = append(out, 0xFF, 0xFE)
		out = binary.BigEndian.AppendUint16(out, uint16(len(c)+2)) // #nosec G115 -- bounded above
		out = append(out, c...)
	}
	return append(out, 0xFF, 0xD9) // EOI
}
