package hls

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// The fragmented MP4 ffmpeg writes, read only as far as cutting it needs:
// which track is the picture and on what clock (moov), and when each
// fragment's picture starts (moof). Nothing is rewritten; a segment is the
// fragments' bytes, verbatim.

var errBox = errors.New("hls: malformed fragmented MP4")

// boxHeader is one ISO BMFF box header read off a stream.
type boxHeader struct {
	typ string
	// size is the whole box, header included; header is the header's length.
	size, header int64
}

// readBoxHeader reads the next top-level box header. io.EOF only at a clean
// box boundary.
func readBoxHeader(r io.Reader) (boxHeader, []byte, error) {
	var h [16]byte
	if _, err := io.ReadFull(r, h[:8]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return boxHeader{}, nil, errBox
		}
		return boxHeader{}, nil, err
	}
	b := boxHeader{typ: string(h[4:8]), size: int64(binary.BigEndian.Uint32(h[:4])), header: 8}
	raw := h[:8]
	if b.size == 1 {
		if _, err := io.ReadFull(r, h[8:16]); err != nil {
			return boxHeader{}, nil, errBox
		}
		large := binary.BigEndian.Uint64(h[8:16])
		if large > math.MaxInt64 {
			return boxHeader{}, nil, errBox
		}
		b.size, b.header, raw = int64(large), 16, h[:16]
	}
	// Size zero ("to the end of the file") never comes out of a fragmenting
	// muxer writing to a pipe; refusing it keeps the reader bounded.
	if b.size < b.header {
		return boxHeader{}, nil, fmt.Errorf("%w: box %q of size %d", errBox, b.typ, b.size)
	}
	return b, append([]byte(nil), raw...), nil
}

// children walks the boxes inside a container's payload.
func children(payload []byte, fn func(typ string, body []byte) error) error {
	for len(payload) > 0 {
		if len(payload) < 8 {
			return errBox
		}
		size := uint64(binary.BigEndian.Uint32(payload[:4]))
		typ, header := string(payload[4:8]), uint64(8)
		if size == 1 {
			if len(payload) < 16 {
				return errBox
			}
			size, header = binary.BigEndian.Uint64(payload[8:16]), 16
		}
		if size < header || size > uint64(len(payload)) {
			return fmt.Errorf("%w: child %q of size %d in %d bytes", errBox, typ, size, len(payload))
		}
		if err := fn(typ, payload[header:size]); err != nil {
			return err
		}
		payload = payload[size:]
	}
	return nil
}

// track is what the segmenter knows of one track from the init segment.
type track struct {
	timescale uint32
	video     bool
}

// parseMoov reads each track's id, clock and kind from a moov payload.
func parseMoov(payload []byte) (map[uint32]track, error) {
	tracks := map[uint32]track{}
	err := children(payload, func(typ string, body []byte) error {
		if typ != "trak" {
			return nil
		}
		var id uint32
		var t track
		err := children(body, func(typ string, body []byte) error {
			switch typ {
			case "tkhd":
				// version(1) flags(3) created modified [32|64 bits each], then track_ID.
				off := 12
				if len(body) > 0 && body[0] == 1 {
					off = 20
				}
				if len(body) < off+4 {
					return errBox
				}
				id = binary.BigEndian.Uint32(body[off : off+4])
			case "mdia":
				return children(body, func(typ string, body []byte) error {
					switch typ {
					case "mdhd":
						off := 12
						if len(body) > 0 && body[0] == 1 {
							off = 20
						}
						if len(body) < off+4 {
							return errBox
						}
						t.timescale = binary.BigEndian.Uint32(body[off : off+4])
					case "hdlr":
						// version/flags(4) pre_defined(4) handler_type(4)
						if len(body) < 12 {
							return errBox
						}
						t.video = string(body[8:12]) == "vide"
					}
					return nil
				})
			}
			return nil
		})
		if err != nil {
			return err
		}
		if id == 0 || t.timescale == 0 {
			return fmt.Errorf("%w: a track with no id or clock", errBox)
		}
		tracks[id] = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tracks, nil
}

// videoStart is when a fragment's picture starts, in seconds on the muxer's
// clock: the video track's base decode time plus its first sample's
// composition offset — the PRESENTATION time of the keyframe that opens the
// fragment, which is the clock ffprobe lists keyframes on. ok is false for a
// fragment with no video samples.
func videoStart(moof []byte, tracks map[uint32]track) (float64, bool, error) {
	var start float64
	found := false
	err := children(moof, func(typ string, body []byte) error {
		if typ != "traf" || found {
			return nil
		}
		var id uint32
		var base uint64
		var cto int64
		haveBase, haveSamples := false, false
		err := children(body, func(typ string, body []byte) error {
			switch typ {
			case "tfhd":
				if len(body) < 8 {
					return errBox
				}
				id = binary.BigEndian.Uint32(body[4:8])
			case "tfdt":
				if len(body) < 8 {
					return errBox
				}
				if body[0] == 1 {
					if len(body) < 12 {
						return errBox
					}
					base = binary.BigEndian.Uint64(body[4:12])
				} else {
					base = uint64(binary.BigEndian.Uint32(body[4:8]))
				}
				haveBase = true
			case "trun":
				if haveSamples {
					return nil // the first run with samples holds the first sample
				}
				n, off, err := firstSampleCTO(body)
				if err != nil {
					return err
				}
				haveSamples, cto = n > 0, off
			}
			return nil
		})
		if err != nil {
			return err
		}
		t, ok := tracks[id]
		if !ok || !t.video || !haveBase || !haveSamples {
			return nil
		}
		start = (float64(base) + float64(cto)) / float64(t.timescale)
		found = true
		return nil
	})
	return start, found, err
}

// firstSampleCTO reads a trun: how many samples it has and the first one's
// composition time offset (zero when the run carries none).
func firstSampleCTO(body []byte) (uint32, int64, error) {
	if len(body) < 8 {
		return 0, 0, errBox
	}
	version := body[0]
	flags := uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
	count := binary.BigEndian.Uint32(body[4:8])
	off := 8
	if flags&0x1 != 0 { // data_offset
		off += 4
	}
	if flags&0x4 != 0 { // first_sample_flags
		off += 4
	}
	if count == 0 || flags&0x800 == 0 {
		return count, 0, nil
	}
	for _, bit := range []uint32{0x100, 0x200, 0x400} {
		if flags&bit != 0 {
			off += 4
		}
	}
	if len(body) < off+4 {
		return 0, 0, errBox
	}
	raw := binary.BigEndian.Uint32(body[off : off+4])
	if version == 1 {
		// #nosec G115 -- a version 1 offset IS a signed 32-bit field; the
		// conversion is the two's-complement reading the format specifies.
		return count, int64(int32(raw)), nil
	}
	return count, int64(raw), nil
}
