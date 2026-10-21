package worker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
)

// A release whose payload is a program (#819).
//
// Fake releases are named like the episode or film they claim to be and carry
// one Windows executable. The title parses as an ordinary 1080p WEB release, so
// every profile gate passes, and before this check the bytes were ingested as
// an asset: a want reported AVAILABLE on a file that plays nothing and that a
// client could hand to someone who opens it.
//
// The check is a verdict on the RELEASE, never on this node, so it blocks the
// release rather than taking #813's node-local no-block edge: the next search
// must pick something else.
//
// A game is exempt. Its payload is installers by definition (ADR-0120).

// errExecutablePayload is the stable reason a release is refused for being a
// program. It leads the failure detail, so an operator reading a block, an
// event or a want's detail sees `payload_executable:` first.
var errExecutablePayload = errors.New("payload_executable")

// hasExecutableExt reports whether name ends in an executable's extension.
func hasExecutableExt(name string) bool { return identification.IsExecutableName(name) }

// executableMagic is what the first bytes of a program look like: a PE
// (Windows, and .com/.scr in practice) starts with MZ, an ELF with 0x7f ELF.
// No video, audio or book container starts with either.
var executableMagic = [][]byte{[]byte("MZ"), []byte("\x7fELF")}

// sniffExecutable reports why path is a program, or "" when it is not: by its
// extension first, then by its first bytes, so a program renamed to .mkv is
// still caught. A file that cannot be read is not judged here; verifyFile has
// already proven it readable.
func sniffExecutable(path string) string {
	if hasExecutableExt(path) {
		return "its extension is " + strings.ToLower(filepath.Ext(path))
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 4)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return ""
	}
	for _, m := range executableMagic {
		if bytes.HasPrefix(head[:n], m) {
			return "its contents are a program"
		}
	}
	return ""
}

// namesForDetail lists a few file names for a failure detail, not a season
// pack's worth.
func namesForDetail(names []string) string {
	const shown = 3
	if len(names) <= shown {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:shown], ", "), len(names)-shown)
}
