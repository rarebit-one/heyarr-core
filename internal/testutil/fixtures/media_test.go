package fixtures

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// These tests are the whole reason the builders are trustworthy. There is no
// ffprobe on the build machines or on the deployment host, so "it looks like an
// MP4" is not available as evidence — the structure is parsed back instead, and
// each test asserts exactly the claim its builder's comment makes.

type mp4Box struct {
	kind string
	size uint32
	body []byte
}

func parseBoxes(t *testing.T, data []byte) []mp4Box {
	t.Helper()
	var boxes []mp4Box
	for len(data) > 0 {
		if len(data) < 8 {
			t.Fatalf("trailing %d bytes are too short to be a box header", len(data))
		}
		size := binary.BigEndian.Uint32(data[0:4])
		if size < 8 || int(size) > len(data) {
			t.Fatalf("box %q declares size %d with %d bytes remaining", data[4:8], size, len(data))
		}
		boxes = append(boxes, mp4Box{kind: string(data[4:8]), size: size, body: data[8:size]})
		data = data[size:]
	}
	return boxes
}

func TestMP4BoxStructureRoundTrips(t *testing.T) {
	payload := []byte("some payload bytes")
	boxes := parseBoxes(t, MP4(payload))

	var kinds []string
	for _, b := range boxes {
		kinds = append(kinds, b.kind)
	}
	want := []string{"ftyp", "free", "mdat"}
	if len(kinds) != len(want) {
		t.Fatalf("box types = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("box types = %v, want %v", kinds, want)
		}
	}

	// ISO/IEC 14496-12: ftyp carries a major brand, a minor version and a list
	// of compatible brands, each four characters.
	ftyp := boxes[0].body
	if string(ftyp[0:4]) != "isom" {
		t.Errorf("major brand = %q, want isom", ftyp[0:4])
	}
	if (len(ftyp)-8)%4 != 0 {
		t.Errorf("compatible brands are %d bytes, which is not a whole number of brands", len(ftyp)-8)
	}
	if !bytes.Contains(ftyp[8:], []byte("mp41")) {
		t.Errorf("compatible brands %q do not include mp41", ftyp[8:])
	}

	if !bytes.Equal(boxes[2].body, payload) {
		t.Errorf("mdat payload did not survive: %q", boxes[2].body)
	}
}

func TestMP4DeclaresItsOwnLimits(t *testing.T) {
	// The `free` box is where the builder writes its own disclaimer, so that
	// anyone who opens the file in a hex editor learns what it is before they
	// spend an afternoon on why it will not play.
	boxes := parseBoxes(t, MP4([]byte("x")))
	if !bytes.Contains(boxes[1].body, []byte("not decodable")) {
		t.Errorf("the free box does not say the file is not decodable: %q", boxes[1].body)
	}
	for _, b := range boxes {
		if b.kind == "moov" {
			t.Fatal("an moov box appeared — if this fixture became decodable, media.go's comments are now wrong")
		}
	}
}

// A parser walking frame to frame must land exactly on each sync word. That is
// the only claim MP3's comment makes, and it is the one that would break if the
// frame-length arithmetic were wrong.
func TestMP3FramesAreWalkable(t *testing.T) {
	const frames = 25
	data := MP3(frames)
	frameLen := MP3FrameLength()

	if got, want := len(data), frames*frameLen; got != want {
		t.Fatalf("length = %d, want %d", got, want)
	}

	seen := 0
	for off := 0; off < len(data); off += frameLen {
		if data[off] != 0xFF || data[off+1]&0xE0 != 0xE0 {
			t.Fatalf("frame %d at offset %d does not start with a sync word: %#x %#x",
				seen, off, data[off], data[off+1])
		}
		// MPEG-1 (bits 4-3 = 11), Layer III (bits 2-1 = 01).
		if version := (data[off+1] >> 3) & 0x03; version != 0x03 {
			t.Fatalf("frame %d declares MPEG version bits %02b, want 11 (MPEG-1)", seen, version)
		}
		if layer := (data[off+1] >> 1) & 0x03; layer != 0x01 {
			t.Fatalf("frame %d declares layer bits %02b, want 01 (Layer III)", seen, layer)
		}
		if idx := (data[off+2] >> 4) & 0x0F; mpegBitrates[idx] == 0 {
			t.Fatalf("frame %d declares bitrate index %d, which is free or reserved", seen, idx)
		}
		if rate := (data[off+2] >> 2) & 0x03; rate == 0x03 {
			t.Fatalf("frame %d declares the reserved sample-rate index", seen)
		}
		seen++
	}
	if seen != frames {
		t.Fatalf("walked %d frames, want %d", seen, frames)
	}
}

func TestMP3FrameLengthMatchesTheStandardFormula(t *testing.T) {
	// 144 * bitrate / sample rate, the form the standard is usually quoted in.
	want := 144 * (mpegBitrates[mp3BitrateIndex] * 1000) / mp3SampleRate
	if got := MP3FrameLength(); got != want {
		t.Fatalf("MP3FrameLength = %d, want %d", got, want)
	}
	if MP3FrameLength() != 417 {
		t.Fatalf("128 kbit/s at 44.1 kHz is 417 bytes per frame, got %d", MP3FrameLength())
	}
}

func TestFLACStreamInfoIsReadable(t *testing.T) {
	data := FLAC([]byte("padding"))

	if string(data[0:4]) != "fLaC" {
		t.Fatalf("magic = %q, want fLaC", data[0:4])
	}
	if data[4]&0x80 == 0 {
		t.Error("STREAMINFO is not marked as the last metadata block")
	}
	if blockType := data[4] & 0x7F; blockType != 0 {
		t.Errorf("first metadata block type = %d, want 0 (STREAMINFO)", blockType)
	}
	length := int(data[5])<<16 | int(data[6])<<8 | int(data[7])
	if length != 34 {
		t.Fatalf("STREAMINFO length = %d, want 34", length)
	}

	info := data[8 : 8+34]
	packed := binary.BigEndian.Uint64(info[10:18])
	if rate := packed >> 44 & 0xFFFFF; rate != 44100 {
		t.Errorf("sample rate = %d, want 44100", rate)
	}
	if channels := (packed>>41)&0x07 + 1; channels != 2 {
		t.Errorf("channels = %d, want 2", channels)
	}
	if bits := (packed>>36)&0x1F + 1; bits != 16 {
		t.Errorf("bits per sample = %d, want 16", bits)
	}
}

func TestMatroskaDocTypeIsReadable(t *testing.T) {
	data := Matroska([]byte("payload"))

	if !bytes.Equal(data[0:4], []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		t.Fatalf("EBML magic = % x, want 1a 45 df a3", data[0:4])
	}
	headerLen := int(data[4] &^ 0x80)
	header := data[5 : 5+headerLen]

	// DocType is element id 0x4282, a one-byte length, then the string.
	i := bytes.Index(header, []byte{0x42, 0x82})
	if i < 0 {
		t.Fatal("no DocType element in the EBML header")
	}
	n := int(header[i+2] &^ 0x80)
	if got := string(header[i+3 : i+3+n]); got != "matroska" {
		t.Fatalf("DocType = %q, want matroska", got)
	}

	segment := data[5+headerLen:]
	if !bytes.Equal(segment[0:4], []byte{0x18, 0x53, 0x80, 0x67}) {
		t.Fatalf("Segment id = % x, want 18 53 80 67", segment[0:4])
	}
}

func TestJPEGMarkersAreWellFormed(t *testing.T) {
	data := JPEG("a comment")

	if !bytes.Equal(data[0:2], []byte{0xFF, 0xD8}) {
		t.Fatalf("does not start with SOI: % x", data[0:2])
	}
	if !bytes.Equal(data[len(data)-2:], []byte{0xFF, 0xD9}) {
		t.Fatalf("does not end with EOI: % x", data[len(data)-2:])
	}
	if !bytes.Equal(data[2:4], []byte{0xFF, 0xE0}) {
		t.Fatalf("APP0 does not follow SOI: % x", data[2:4])
	}
	if string(data[6:10]) != "JFIF" {
		t.Fatalf("APP0 is not JFIF: %q", data[6:10])
	}
	// Every segment length must cover itself, or a reader walking markers
	// desynchronises.
	if got := binary.BigEndian.Uint16(data[4:6]); int(got) != 16+2-2 {
		t.Errorf("APP0 length = %d, want 16", got)
	}
	if !bytes.Contains(data, []byte("a comment")) {
		t.Error("the comment did not survive")
	}
}

func TestJPEGWithoutACommentIsStillValid(t *testing.T) {
	data := JPEG("")
	if !bytes.Equal(data[len(data)-2:], []byte{0xFF, 0xD9}) {
		t.Fatalf("does not end with EOI: % x", data[len(data)-2:])
	}
	if bytes.Contains(data[4:], []byte{0xFF, 0xFE}) {
		t.Error("a comment segment was emitted for an empty comment")
	}
}
