package fixtures

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The JPEG builder is still hand-written, so it is still parsed back here: it
// is a header with no scan data and nothing in Milestone 2 decodes it, which
// is what keeps it honest rather than a fixture pretending to be an image.
//
// The video and audio fixtures are no longer built by hand and are no longer
// tested by re-parsing them. They are real encoded files, and the only
// assertion worth making about a real encoded file is what a decoder says
// about it — which is media_probe_test.go, running wherever the pinned
// toolchain is installed.

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
