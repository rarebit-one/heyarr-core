package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/void-which-binds-go/hashing"
)

var errMusicMatch = errors.New("music metadata match is missing or ambiguous")

type musicContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r musicContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// inspectMusicMetadata copies the source into a private staging file while
// hashing, then checks the copy after ffprobe runs. The result is not installed
// as a blob. The ingester checks the actual CAS bytes against this digest before
// recording the asset, including when Link deduplicates to an existing blob.
func inspectMusicMetadata(ctx context.Context, stagingDir, path string) (string, map[string]string, error) {
	if stagingDir == "" {
		return "", nil, errors.New("music inspection has no managed staging directory")
	}
	stage, err := os.CreateTemp(stagingDir, "music-track-*.part")
	if err != nil {
		return "", nil, fmt.Errorf("creating private staging file: %w", err)
	}
	defer os.Remove(stage.Name())
	defer stage.Close()
	source, err := os.Open(path)
	if err != nil {
		return "", nil, fmt.Errorf("opening source: %w", err)
	}
	hasher := hashing.New()
	_, err = io.Copy(io.MultiWriter(stage, hasher), musicContextReader{ctx: ctx, reader: source})
	closeErr := source.Close()
	if err != nil {
		return "", nil, fmt.Errorf("staging source: %w", err)
	}
	if closeErr != nil {
		return "", nil, closeErr
	}
	expected := hasher.Sum()
	if err = stage.Close(); err != nil {
		return "", nil, fmt.Errorf("closing staging file: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	cmd := exec.CommandContext(probeCtx, "ffprobe", "-v", "error", "-show_entries", "format_tags", "-of", "json", stage.Name())
	output, err := cmd.Output()
	timedOut := probeCtx.Err()
	cancel()
	if err != nil {
		if timedOut != nil {
			return "", nil, fmt.Errorf("probing music metadata: %w", timedOut)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", nil, fmt.Errorf("%w: unreadable audio metadata", errMusicMatch)
		}
		return "", nil, fmt.Errorf("running ffprobe: %w", err)
	}
	recheck, err := os.Open(stage.Name())
	if err != nil {
		return "", nil, fmt.Errorf("opening staged bytes for recheck: %w", err)
	}
	actual, _, err := hashing.HashReader(musicContextReader{ctx: ctx, reader: recheck})
	closeErr = recheck.Close()
	if err != nil {
		return "", nil, fmt.Errorf("rechecking staged bytes: %w", err)
	}
	if closeErr != nil {
		return "", nil, closeErr
	}
	if actual != expected {
		return "", nil, errors.New("staged bytes changed while inspecting music metadata")
	}
	var parsed struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output, &parsed); err != nil {
		return "", nil, fmt.Errorf("decoding ffprobe metadata: %w", err)
	}
	return expected.String(), parsed.Format.Tags, nil
}

// selectMusicArtifact requires one unique embedded metadata match across the
// whole release. This is metadata matching, not acoustic verification.
func selectMusicArtifact(ctx context.Context, p *ingest.Pipeline, batch verifiedTransfer, title, artist string) (verifiedTransfer, error) {
	if len(batch.artifacts) > 200 {
		return batch, fmt.Errorf("%w: release has too many files to inspect safely", errMusicMatch)
	}
	stagingDir, err := p.StagingDir()
	if err != nil {
		return batch, err
	}
	var matches []artifact
	for _, a := range batch.artifacts {
		if identification.MusicAlternateVersion(filepath.Base(batch.path)+" "+a.RelPath, title, artist) {
			continue
		}
		hash, tags, err := inspectMusicMetadata(ctx, stagingDir, a.Path)
		if err != nil {
			return batch, err
		}
		if ok, _ := identification.MatchMusicTags(title, artist, tags); ok {
			a.expectedBlobHash = hash
			matches = append(matches, a)
		}
	}
	if len(matches) != 1 {
		return batch, fmt.Errorf("%w: %d matching tracks; expected exactly one", errMusicMatch, len(matches))
	}
	batch.artifacts = matches
	batch.pack = false
	return batch, nil
}
