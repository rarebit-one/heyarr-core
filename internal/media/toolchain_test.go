package media_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/media"
)

// fakeTool writes an executable that answers -version like the named tool,
// printing the given banner. It is a real file executed by a real exec.Command:
// the thing under test is whether Resolve can run a binary and believe it, and
// a stub that returned a string would assert only that the test's idea of that
// is self-consistent.
func fakeTool(t *testing.T, name, banner string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake tools are shell scripts; the resolver itself is platform-neutral")
	}
	path := filepath.Join(t.TempDir(), name)
	script := fmt.Sprintf("#!/bin/sh\ncat <<'BANNER'\n%s\nBANNER\n", banner)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // a test fixture that must be executable
		t.Fatal(err)
	}
	return path
}

// silentTool writes an executable that exits 0 and prints nothing — /bin/true
// wearing a name. It is the case that makes "we found something" and "we found
// ffprobe" different questions.
func silentTool(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake tools are shell scripts; the resolver itself is platform-neutral")
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // a test fixture that must be executable
		t.Fatal(err)
	}
	return path
}

const ffprobeBanner = `ffprobe version 6.1.1 Copyright (c) 2007-2023 the FFmpeg developers
built with Apple clang version 15.0.0`

const ffmpegBanner = `ffmpeg version 6.1.1 Copyright (c) 2000-2023 the FFmpeg developers
built with Apple clang version 15.0.0`

func TestResolveReportsBothToolsAndTheirCapabilities(t *testing.T) {
	tc, err := media.Resolve(t.Context(), media.Options{
		FFprobePath: fakeTool(t, "ffprobe", ffprobeBanner),
		FFmpegPath:  fakeTool(t, "ffmpeg", ffmpegBanner),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !tc.FFprobe.Available || tc.FFprobe.Version != "6.1.1" {
		t.Errorf("ffprobe = %+v", tc.FFprobe)
	}
	if !tc.FFmpeg.Available || tc.FFmpeg.Version != "6.1.1" {
		t.Errorf("ffmpeg = %+v", tc.FFmpeg)
	}
	if got := tc.Capabilities(); len(got) != 2 ||
		got[0] != media.CapabilityFFprobe || got[1] != media.CapabilityFFmpeg {
		t.Errorf("capabilities = %v, want [ffprobe ffmpeg]", got)
	}
}

// The degrade path, and the reason the whole package exists: a node with no
// toolchain resolves cleanly, advertises nothing, and is a supported
// configuration rather than a startup failure.
func TestAnAbsentToolchainIsNotAnError(t *testing.T) {
	tc, err := media.Resolve(t.Context(), media.NoToolchain())
	if err != nil {
		t.Fatalf("resolving on a machine with no toolchain failed: %v", err)
	}
	if tc.FFprobe.Available || tc.FFmpeg.Available {
		t.Fatalf("something was reported available with no PATH: %+v", tc)
	}
	if got := tc.Capabilities(); len(got) != 0 {
		t.Errorf("capabilities = %v on a bare node, want none", got)
	}
	for _, tool := range tc.Tools() {
		if tool.Detail == "" {
			t.Errorf("%s is unavailable and says nothing about why", tool.Name)
		}
	}
}

// A configured path is a statement of intent, and it is not symmetrical with
// PATH discovery. Nobody asked for the PATH copy; someone asked for this one.
func TestAConfiguredPathThatDoesNotExistIsAStartupError(t *testing.T) {
	_, err := media.Resolve(t.Context(), media.Options{
		FFprobePath: filepath.Join(t.TempDir(), "definitely-not-here"),
	})
	if err == nil {
		t.Fatal("a configured ffprobe path that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "ffprobe") {
		t.Errorf("the error does not name the tool: %v", err)
	}
}

// The /bin/true case. A binary that exits 0 and says nothing must not resolve
// to an available tool with an empty version — that failure surfaces at the
// first probe, looking like a probing bug rather than a configuration one.
func TestASilentBinaryIsNotAToolchain(t *testing.T) {
	if _, err := media.Resolve(t.Context(), media.Options{
		FFprobePath: silentTool(t, "ffprobe"),
	}); err == nil {
		t.Fatal("a configured binary that reports no version was accepted")
	}

	// Found on PATH rather than configured, the same binary degrades instead
	// of refusing to start: nobody asked for it, so it is not an error that it
	// is useless.
	tc, err := media.Resolve(t.Context(), media.WithLookupResult(silentTool(t, "ffprobe"), ""))
	if err != nil {
		t.Fatalf("an unusable binary merely found on PATH failed startup: %v", err)
	}
	if tc.FFprobe.Available {
		t.Error("a binary that reports no version was marked available")
	}
	if tc.FFprobe.Detail == "" {
		t.Error("an unusable PATH binary says nothing about why")
	}
}

// One tool present and the other missing is the ordinary partial case: a node
// that can probe but not remux is useful, and must advertise exactly what it
// can do.
func TestOneToolPresentAdvertisesOnlyThatCapability(t *testing.T) {
	tc, err := media.Resolve(t.Context(),
		media.WithLookupResult("", fakeTool(t, "ffprobe", ffprobeBanner)))
	if err != nil {
		t.Fatal(err)
	}
	got := tc.Capabilities()
	if len(got) != 1 || got[0] != media.CapabilityFFprobe {
		t.Fatalf("capabilities = %v, want [ffprobe] only", got)
	}
	if tc.FFmpeg.Available {
		t.Error("ffmpeg is available with nothing on PATH")
	}
}

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		banner string
		want   string
		err    bool
	}{
		{"release", ffprobeBanner, "6.1.1", false},
		{"git build", "ffmpeg version n7.0 Copyright (c)", "7.0", false},
		{"distro suffix", "ffprobe version 4.4.2-0ubuntu0.22.04.1 Copyright", "4.4.2-0ubuntu0.22.04.1", false},
		{"empty", "", "", true},
		{"not a banner", "hello\n", "", true},
		{"missing the word version", "ffprobe 6.1.1 Copyright", "", true},
		{"truncated", "ffprobe version\n", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := media.ParseVersionForTest(tc.banner)
			if tc.err {
				if err == nil {
					t.Fatalf("parsed %q as %q, want an error", tc.banner, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("= %q, want %q", got, tc.want)
			}
		})
	}
}

// Against the actual pinned binaries, when they are installed.
//
// Everything above uses shell scripts that print a banner, which proves the
// resolver's logic and nothing about FFmpeg. This proves the two agree: that a
// real ffprobe's real banner parses, and that the version matches what
// scripts/toolchain.lock claims is installed. A pin whose recorded version has
// never been compared to the binary is a comment, not a pin.
//
// Skipped when the toolchain is absent, which is a supported state — and CI
// asserts on the Linux runners that this did NOT skip, because a skipped test
// and a passing one look identical in the summary line.
func TestAgainstTheRealPinnedToolchain(t *testing.T) {
	dir := os.Getenv("HEYARR_TEST_TOOLCHAIN_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", ".toolchain", "bin")
	}
	probePath := filepath.Join(dir, "ffprobe")
	if _, err := os.Stat(probePath); err != nil {
		t.Skip("no pinned toolchain installed; run scripts/toolchain.sh")
	}

	tc, err := media.Resolve(t.Context(), media.Options{
		FFprobePath: probePath,
		FFmpegPath:  filepath.Join(dir, "ffmpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !tc.FFprobe.Available || !tc.FFmpeg.Available {
		t.Fatalf("the pinned toolchain did not resolve: %+v", tc)
	}

	want, err := lockedVersion(t, "ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	if tc.FFprobe.Version != want {
		t.Errorf("ffprobe reports %q; scripts/toolchain.lock says %q for this platform. "+
			"The lock is wrong about the version, the digest, or both",
			tc.FFprobe.Version, want)
	}
	t.Logf("pinned toolchain: ffprobe %s, ffmpeg %s", tc.FFprobe.Version, tc.FFmpeg.Version)
}

// lockedVersion reads the version this platform's entry pins.
func lockedVersion(t *testing.T, tool string) (string, error) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "toolchain.lock"))
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%s-%s-%s", tool, runtime.GOOS, runtime.GOARCH)
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == key {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("scripts/toolchain.lock has no entry for %s", key)
}
