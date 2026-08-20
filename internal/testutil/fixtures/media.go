package fixtures

import (
	"embed"
	"encoding/binary"
	"math"
)

// The media fixtures are real files that a real decoder has really opened.
//
// # What changed, and why the old note mattered
//
// Milestone 1's builders produced structurally valid containers that no decoder
// had ever opened, and said so at the top of this file. The MP4 had no `moov`,
// so no track and no codec; the MP3 frames were silent; the FLAC had a
// STREAMINFO and no subframes. That was deliberate: building a "decodable" MP4
// by hand on a machine with no ffprobe anywhere to check it against would have
// bought false confidence rather than coverage, and the bill would have arrived
// in Milestone 2 looking like a probing bug rather than a fixture bug.
//
// The note is kept rather than deleted because the reasoning is why the debt
// was survivable, and because the same trap is available to anyone adding a
// format Heyarr cannot yet decode.
//
// Milestone 2 has a decoder (ADR-0023), so these are now genuine: encoded by
// FFmpeg from synthetic sources, committed, and asserted against ffprobe in
// media_test.go wherever the toolchain is installed.
//
// # Committed, not generated
//
// The bytes are in the repository, so `go test ./...` needs no external binary
// and the pure-Go suite stays hermetic. scripts/genmedia.sh regenerates them
// and is only run deliberately.
//
// Everything is synthetic — testsrc2 and sine — so the bytes are ours and the
// licensing of a public AGPL repository stays simple. No third-party samples.
//
// # What is still not decodable, on purpose
//
// JPEG below is a header with no scan data, and the large streaming fixture in
// fixtures.go is a real Matroska header followed by pseudorandom bytes. Neither
// is decoded by anything: the first exists so the artwork asset role has a file
// with the right magic, and the second exists to be range-served at gigabyte
// scale, which real encoded video cannot be without putting a gigabyte in git.
// Both say so where they are defined, which is the part Milestone 1 got right.

//go:embed media/*.mp4 media/*.mkv media/*.flac media/*.mp3
var mediaFS embed.FS

// The committed samples are roughly one second at 160x120, or two seconds of a
// tone: they are hashed, ranged over and copied by a large fraction of the test
// suite, so they are not a place to put megabytes.
//
// The set spans what the playback planner (§68, M2-07) has to tell apart,
// because a planner tested only against files that happen to exist is a planner
// tested against one case:
//
//	SampleMP4     h264 + aac in mp4       — DIRECT for almost any device
//	SampleMKV     the same streams in mkv — REMUX: right codecs, wrong container
//	SampleHEVCMP4 hevc + aac in mp4       — TRANSCODE for a device without HEVC
//	SampleFLAC    flac                    — lossless audio
//	SampleMP3     mp3                     — lossy audio

// SampleMP4 is H.264 video and AAC audio in an MP4 container.
//
// variant selects between two files that differ in CONTENT rather than by
// appended padding. The ingest fixtures need files that must not deduplicate,
// and padding a real container to make it distinct is how a fixture stops being
// a representative of the thing it stands for.
func SampleMP4(variant int) []byte {
	return mediaFile("media/h264_aac_" + variantSuffix(variant) + ".mp4")
}

// SampleMKV is SampleMP4's streams in Matroska — the same video and audio, a
// different container. That pairing is the REMUX case, and having both means a
// test can assert that a remux changed the container and nothing else.
func SampleMKV(variant int) []byte {
	return mediaFile("media/h264_aac_" + variantSuffix(variant) + ".mkv")
}

// SampleHEVCMP4 is HEVC video, which an ordinary device profile refuses.
func SampleHEVCMP4() []byte { return mediaFile("media/hevc_aac.mp4") }

// SampleFLAC is two seconds of a 440 Hz tone, losslessly encoded.
func SampleFLAC() []byte { return mediaFile("media/flac.flac") }

// SampleMP3 is two seconds of a 330 Hz tone at 128 kbit/s.
func SampleMP3() []byte { return mediaFile("media/mp3.mp3") }

// variantSuffix maps a variant to a file. Anything outside the set wraps rather
// than panicking, so a caller asking for variant 7 gets a real file instead of
// a test failure that has nothing to do with what it was testing.
func variantSuffix(variant int) string {
	if variant%2 == 0 {
		return "2"
	}
	return "1"
}

// mediaFile reads an embedded sample.
//
// It panics on a missing file, which is correct here and nowhere else: the
// files are embedded at build time, so a miss means the embed pattern and the
// accessor have drifted apart, and every caller is a test that would otherwise
// fail somewhere far less informative.
func mediaFile(name string) []byte {
	b, err := mediaFS.ReadFile(name)
	if err != nil {
		panic("fixtures: embedded media is missing: " + err.Error())
	}
	// A copy, so a caller that appends to or mutates the slice cannot corrupt
	// the embedded bytes for every later test in the process.
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// MatroskaHeader is a Matroska EBML header with no clusters, used as the prefix
// of the large streaming fixture.
//
// It is deliberately NOT decodable, and that is not the same mistake Milestone
// 1 made. The streaming fixture is gigabytes of pseudorandom bytes whose whole
// purpose is to be range-served without putting gigabytes in git; there is no
// version of it that is also real video. What matters is that a file named
// .mkv identifies as Matroska rather than as "data", which is exactly where the
// range assertions look.
func MatroskaHeader() []byte {
	ebml := []byte{
		0x42, 0x86, 0x81, 0x01, // EBMLVersion 1
		0x42, 0xF7, 0x81, 0x01, // EBMLReadVersion 1
		0x42, 0xF2, 0x81, 0x04, // EBMLMaxIDLength 4
		0x42, 0xF3, 0x81, 0x08, // EBMLMaxSizeLength 8
		0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a',
		0x42, 0x87, 0x81, 0x04, // DocTypeVersion 4
		0x42, 0x85, 0x81, 0x02, // DocTypeReadVersion 2
	}
	out := []byte{0x1A, 0x45, 0xDF, 0xA3} // EBML header id
	// #nosec G115 -- ebml is a fixed literal above, 33 bytes, and an EBML
	// one-byte length can carry up to 127.
	out = append(out, 0x80|byte(len(ebml)))
	out = append(out, ebml...)
	// An unknown-length Segment, which is what a live or streamed Matroska file
	// legitimately looks like.
	return append(out, 0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
}

// JPEG builds a baseline JPEG header: SOI, a real JFIF APP0 segment, an
// optional comment, then EOI.
//
// There is no scan data, so there is no image. It carries the extension, the
// magic bytes and enough structure for the artwork asset role — and unlike the
// Milestone 1 video builders, nothing in Milestone 2 decodes it, so it is not
// a fixture pretending to be something a decoder would accept.
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
