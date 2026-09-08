package ffmpeg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// A video often carries its subtitles INSIDE the container as text tracks
// (mov_text in an .mp4, SubRip/ASS in an .mkv) rather than beside it as an .srt
// sidecar. Heyarr's caption path — the in-app player and the DLNA
// CaptionInfo.sec header — only knows how to serve a subtitle that is its own
// `role='subtitle'` asset (a blob with a whitelisted text MIME). So an embedded
// track is invisible to every consumer until it is lifted out into one.
//
// This lifts it out: enumerate the embedded subtitle streams, and for each TEXT
// one write a standalone SubRip (.srt) the CAS adopts as an ordinary subtitle
// asset. After that the existing resolver picks it up with no change — it is
// byte-for-byte the same shape as a sidecar the release shipped.
//
// # Text only, on purpose
//
// A container can also carry BITMAP subtitles — PGS (Blu-ray), VobSub (DVD),
// DVB. Those are images of text, not text; turning them into an .srt needs OCR,
// which is a different feature with different failure modes (wrong characters,
// not a failed job). This extracts the codecs FFmpeg can losslessly rewrite
// into SubRip and skips the rest, so a disc rip yields whatever text tracks it
// has and silently ignores the picture ones rather than producing garbage.

// ExtractJobType is the queue's name for an embedded-subtitle extraction.
const ExtractJobType = "extract_subtitles"

// ExtractPayload is the extract_subtitles job payload.
type ExtractPayload struct {
	// BlobHash is the video whose embedded tracks are lifted out.
	BlobHash string `json:"blob_hash"`
	// AssetID is the video asset, so each extracted subtitle can be attached to
	// the same Edition (mirrors the remux payload's AssetID).
	AssetID string `json:"asset_id"`
}

// ExtractDedupeKey makes the job idempotent: two scans of the same video while
// the first extraction is still live yield one job (ADR-0008).
func ExtractDedupeKey(blobHash string) string { return ExtractJobType + ":" + blobHash }

// textSubtitleCodecs is the set FFmpeg can rewrite into SubRip with `-c:s srt`.
//
// An allowlist, not a denylist: an unknown codec is skipped rather than fed to
// a conversion that might fail or emit nonsense. The bitmap formats (PGS,
// VobSub/dvd_subtitle, DVB) are deliberately absent — they are images and need
// OCR, which this does not do.
var textSubtitleCodecs = map[string]bool{
	"subrip":     true,
	"srt":        true,
	"ass":        true,
	"ssa":        true,
	"mov_text":   true, // the common one inside .mp4 (the Yellowstone case)
	"webvtt":     true,
	"text":       true,
	"subviewer":  true,
	"subviewer1": true,
	"microdvd":   true,
	"mpl2":       true,
	"jacosub":    true,
	"sami":       true,
	"realtext":   true,
	"pjs":        true,
	"vplayer":    true,
	"stl":        true,
}

// IsTextSubtitleCodec reports whether an FFmpeg subtitle codec is text (and so
// convertible to SubRip) rather than a bitmap format needing OCR.
func IsTextSubtitleCodec(codec string) bool {
	return textSubtitleCodecs[strings.ToLower(strings.TrimSpace(codec))]
}

// SubtitleStream is one embedded subtitle track worth extracting.
type SubtitleStream struct {
	// Index is the stream's absolute index in the container, for `-map 0:<i>`.
	Index int
	// Codec is FFmpeg's codec_name; always a text one (List filters).
	Codec string
	// Language is the ISO code from the track's tags, "" when untagged.
	Language string
	// Title is the track's title tag, "" when none — a human label like "SDH".
	Title string
	// Forced marks a track meant to show only foreign-dialogue lines.
	Forced bool
}

// ExtractorOptions configure an Extractor. Both binaries are the resolved
// toolchain (ADR-0023): ffprobe enumerates the tracks, ffmpeg lifts them out.
type ExtractorOptions struct {
	FFmpegPath  string
	FFprobePath string
	// WorkDir is where an extracted .srt is written before the store adopts it;
	// inside the data directory so adoption is metadata, not a copy (ADR-0014).
	WorkDir string
	Timeout time.Duration
	Logger  *slog.Logger
}

// Extractor lifts embedded text subtitle tracks out of a video into SubRip.
type Extractor struct {
	ffmpeg  string
	ffprobe string
	workDir string
	timeout time.Duration
	log     *slog.Logger
}

// NewExtractor builds an Extractor, refusing a mis-wired one at construction.
func NewExtractor(opts ExtractorOptions) (*Extractor, error) {
	if opts.FFmpegPath == "" {
		return nil, errors.New("ffmpeg: an ffmpeg path is required to extract subtitles")
	}
	if opts.FFprobePath == "" {
		return nil, errors.New("ffmpeg: an ffprobe path is required to enumerate subtitle tracks")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		// A subtitle track is small; even a long film's is seconds to rewrite.
		// Generous anyway, so a slow spindle is not mistaken for a hang.
		timeout = 15 * time.Minute
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Extractor{
		ffmpeg: opts.FFmpegPath, ffprobe: opts.FFprobePath,
		workDir: opts.WorkDir, timeout: timeout,
		log: log.With("component", "ffmpeg-subs"),
	}, nil
}

// ffprobe JSON shapes, only the fields List reads.
type ffprobeStreams struct {
	Streams []ffprobeStream `json:"streams"`
}

type ffprobeStream struct {
	Index       int               `json:"index"`
	CodecName   string            `json:"codec_name"`
	CodecType   string            `json:"codec_type"`
	Tags        map[string]string `json:"tags"`
	Disposition map[string]int    `json:"disposition"`
}

// List enumerates the TEXT subtitle tracks in srcPath, in container order.
//
// Bitmap tracks are dropped here rather than failing later: the caller extracts
// exactly what comes back, so the "no OCR" decision lives in one place.
func (e *Extractor) List(ctx context.Context, srcPath string) ([]SubtitleStream, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// #nosec G204 -- the binary is the resolved toolchain (ADR-0023) and the
	// only non-literal argument is a path this process was handed.
	cmd := exec.CommandContext(ctx, e.ffprobe, probeSubsArgs(srcPath)...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: listing subtitle tracks: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var parsed ffprobeStreams
	if err := json.Unmarshal([]byte(stdout.String()), &parsed); err != nil {
		return nil, fmt.Errorf("ffmpeg: undecodable ffprobe output: %w", err)
	}

	var out []SubtitleStream
	for _, s := range parsed.Streams {
		if s.CodecType != "subtitle" || !IsTextSubtitleCodec(s.CodecName) {
			continue
		}
		out = append(out, SubtitleStream{
			Index:    s.Index,
			Codec:    strings.ToLower(s.CodecName),
			Language: normaliseLang(s.Tags["language"]),
			Title:    strings.TrimSpace(s.Tags["title"]),
			Forced:   s.Disposition["forced"] == 1,
		})
	}
	return out, nil
}

// Extract rewrites one subtitle stream into a standalone SubRip file. The
// caller owns the returned file and must remove it.
func (e *Extractor) Extract(ctx context.Context, srcPath string, stream SubtitleStream) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	out, err := os.CreateTemp(e.workDir, "heyarr-sub-*.srt")
	if err != nil {
		return Result{}, fmt.Errorf("ffmpeg: creating the subtitle output file: %w", err)
	}
	path := out.Name()
	if err := out.Close(); err != nil {
		return Result{}, err
	}

	started := time.Now()
	// #nosec G204 -- resolved toolchain binary; every argument is a literal, a
	// stream index we parsed as an int, or a path this process created.
	cmd := exec.CommandContext(ctx, e.ffmpeg, extractArgs(srcPath, path, stream.Index)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			e.log.Warn("a failed subtitle extraction left a file behind", "path", path, "error", rmErr)
		}
		detail := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("%w: timed out: %s", ErrRemuxFailed, detail)
		}
		return Result{}, fmt.Errorf("%w: %s", ErrRemuxFailed, detail)
	}

	info, err := os.Stat(path)
	if err != nil {
		return Result{}, fmt.Errorf("ffmpeg: the extracted subtitle is not there: %w", err)
	}
	// An empty result is a track that carried no cues — nothing to serve. Treat
	// it as a non-result so the caller does not adopt a zero-byte "subtitle".
	if info.Size() == 0 {
		_ = os.Remove(path)
		return Result{}, fmt.Errorf("%w: the extracted subtitle was empty", ErrRemuxFailed)
	}
	return Result{Path: path, Size: info.Size(), Elapsed: time.Since(started)}, nil
}

// probeSubsArgs enumerates subtitle streams as JSON. Kept here so it can be
// read and tested. `-select_streams s` limits output to subtitle streams.
func probeSubsArgs(src string) []string {
	return []string{
		"-v", "error",
		"-select_streams", "s",
		"-show_entries", "stream=index,codec_name,codec_type:stream_tags=language,title:stream_disposition=forced",
		"-of", "json",
		src,
	}
}

// extractArgs is the command line for one extraction, in one place so it can be
// read and tested.
//
//	-nostdin    a worker has no terminal; without this ffmpeg can block on one.
//	-map 0:<i>  take exactly the one subtitle stream by its container index.
//	-c:s srt    rewrite it as SubRip — the whitelisted, universally served text
//	            format; converting ASS/mov_text/WebVTT to it loses styling but
//	            gains a caption every player and TV understands.
//	-f srt      name the muxer explicitly rather than trusting the extension.
func extractArgs(src, dst string, index int) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", src,
		"-map", "0:" + strconv.Itoa(index),
		"-c:s", "srt",
		"-f", "srt",
		dst,
	}
}

// normaliseLang tidies an ffprobe language tag: lower-cased, and the "und"
// (undetermined) sentinel folded to empty so it is not shown as a language.
func normaliseLang(v string) string {
	l := strings.ToLower(strings.TrimSpace(v))
	if l == "und" {
		return ""
	}
	return l
}
