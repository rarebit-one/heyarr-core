package downloads

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// GOGClient is a download client that fetches a game the operator OWNS on GOG,
// by running `lgogdownloader` against their library (ADR-0120).
//
// # Why it is a download client
//
// For the reason the yt-dlp client is one: everything downstream of a finished
// transfer — the path map, bundle ingest, hashing — wants files on disk, and
// lgogdownloader produces exactly that. A game want reaches acquisition as a
// direct release tagged acquisition.GOGSourceScheme, and this client is its
// SELECTED → QUEUED edge. It refuses every other source, so it composes with
// the http, torrent and yt-dlp clients rather than competing with them.
//
// # A transfer is only done once GOG has vouched for every file
//
// A store package is many files — an installer and its data parts, the DLC
// installers — and one file of a game is not a copy of it (ADR-0118 §2). So the
// download is followed by `--status`, which checks every file against GOG's own
// file list and, where GOG publishes one (installers do), its MD5, and the
// transfer completes only when every file is OK. A part GOG lists but that is
// absent is always caught; a file GOG publishes no checksum for is reported OK
// by lgogdownloader without comparison (ADR-0120 §3 says exactly what OK
// proves). Anything else — a part missing, a checksum mismatch, a short file — fails
// the transfer with the counts, and nothing reaches ingest. That is what lets
// ingest record the edition as a verified store package.
//
// # The account is lgogdownloader's, not Heyarr's
//
// lgogdownloader keeps its login, platform and language choices in its own
// config directory under the service user's home. Heyarr holds no GOG
// credential: an operator logs in once (`lgogdownloader --login` as the service
// user), the way a torrent daemon is configured outside Heyarr. A missing
// binary or an expired login surfaces as the transfer's Error.
type GOGClient struct {
	name    string
	dir     string
	label   string
	timeout time.Duration
	run     GOGRunner
	now     func() time.Time

	// slot lets one game fetch run at a time. A game is tens of gigabytes:
	// fetching several at once only splits the same link and disk between
	// them, and a backfill of a whole library would start every one together.
	slot chan struct{}

	mu        sync.Mutex
	transfers map[string]*providers.Transfer
}

// GOGFileStatus is one line of `lgogdownloader --status`: a status code for one
// file of the game's package.
type GOGFileStatus struct {
	// Code is OK, ND (not downloaded), MD5 (checksum mismatch) or FS (size
	// mismatch, an incomplete download).
	Code string
	// File is the file's name.
	File string
	// Size is the file's size on disk, when lgogdownloader printed one.
	Size int64
}

// GOGRunner runs lgogdownloader. It is injected so the Add → fetch → verify
// wiring is provable without the tool or a GOG account (ADR-0026: the real
// runner is exercised only against the live tool, never in CI).
//
// Errors must be source-free and safe to log.
type GOGRunner interface {
	// Download fetches gameName's installers into dir.
	Download(ctx context.Context, dir, gameName string) error
	// Status reports every file of gameName's installer package under dir.
	Status(ctx context.Context, dir, gameName string) ([]GOGFileStatus, error)
}

// GOGOptions builds a GOGClient.
type GOGOptions struct {
	// Name is the operator's name for this provider.
	Name string
	// Dir is where packages are written — the local side of the path map.
	Dir string
	// Label tags this instance's transfers. Empty means the default.
	Label string
	// Timeout bounds one fetch and its verification. Zero uses defaultGOGTimeout.
	Timeout time.Duration
	// Runner runs lgogdownloader. Nil uses the real subprocess (execLgog).
	Runner GOGRunner
	// Now is injected so health timestamps are testable.
	Now func() time.Time
}

// defaultGOGTimeout bounds one game: tens of gigabytes on a home link, then a
// full MD5 pass over all of it. Finite so a stuck process cannot pin a transfer
// at DOWNLOADING forever.
const defaultGOGTimeout = 24 * time.Hour

// lgogBinary is the tool this client runs.
const lgogBinary = "lgogdownloader"

// NewGOG builds a GOG download client, refusing a mis-wired one.
func NewGOG(opts GOGOptions) (*GOGClient, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("downloads: a GOG client needs a provider name")
	}
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("downloads: a GOG client needs a download directory (the path_map local side)")
	}
	label := opts.Label
	if label == "" {
		label = DefaultLabel
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultGOGTimeout
	}
	runner := opts.Runner
	if runner == nil {
		runner = execLgog{}
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &GOGClient{
		name: strings.TrimSpace(opts.Name), dir: opts.Dir, label: label,
		timeout: timeout, run: runner, now: now,
		transfers: map[string]*providers.Transfer{},
		slot:      make(chan struct{}, 1),
	}, nil
}

var _ providers.Downloader = (*GOGClient)(nil)

// Name implements providers.Provider.
func (c *GOGClient) Name() string { return c.name }

// Capabilities implements providers.Provider.
func (c *GOGClient) Capabilities() []providers.Capability {
	return []providers.Capability{providers.CapabilityDownload}
}

// Check implements providers.Provider. Like the yt-dlp client there is no daemon
// to be down; whether the tool is installed and logged in is discovered at the
// fetch and reported through the transfer's Error.
func (c *GOGClient) Check(_ context.Context) providers.Health {
	return providers.Healthy(lgogBinary, c.now())
}

// ErrNotGOGSource is the refusal that lets this client compose with the others.
var ErrNotGOGSource = errors.New("downloads: not a GOG store source")

// Add implements providers.Downloader. Idempotent while a fetch is in flight: a
// re-run grab gets the transfer the first one registered (invariant 9).
//
// A transfer that is OVER — failed, or done — is fetched again. A failed one
// for the obvious reason; a done one because the only way a grab reaches a
// finished transfer is a later attempt for the same want, and that attempt
// exists because the earlier package did not stick (ingest refused it, or its
// bytes went missing). Replaying the old "done" would hand ingest the same
// rejected directory forever. Fetching again is cheap: lgogdownloader keeps the
// files that are already right, so it costs a verification, not a download.
func (c *GOGClient) Add(_ context.Context, source secret.Value) (providers.Transfer, error) {
	game, ok := acquisition.ParseGOGSource(source.Reveal())
	if !ok {
		return providers.Transfer{}, ErrNotGOGSource
	}
	// The transfer is the release, which is per want (acquisition.GOGReleaseSource).
	id := strings.TrimSpace(source.Reveal())

	c.mu.Lock()
	if existing, ok := c.transfers[id]; ok && existing.Error == "" && !existing.Done {
		t := *existing
		c.mu.Unlock()
		return t, nil
	}
	t := providers.Transfer{ID: id, Name: game}
	// The map holds its own copy: the fetch goroutine mutates the stored transfer
	// under the lock, and the value returned here must not be that same memory.
	stored := t
	c.transfers[id] = &stored
	c.mu.Unlock()

	//nolint:gosec // G118: the fetch deliberately outlives the grab job's context; it runs on
	// a detached, timeout-bounded context, exactly like the yt-dlp client's.
	go c.fetch(id, game)
	return t, nil
}

// fetch downloads one game into its own directory and verifies it. The
// directory is per game so a second game's files never sit in the bundle ingest
// walks for the first.
func (c *GOGClient) fetch(id, game string) {
	// Wait for the slot before the clock starts: the timeout bounds this
	// game's own fetch, not the queue in front of it.
	c.slot <- struct{}{}
	defer func() { <-c.slot }()

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	dir := filepath.Join(c.dir, "gog-"+game)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		c.fail(id, "mkdir: "+err.Error())
		return
	}
	if err := c.run.Download(ctx, dir, game); err != nil {
		c.fail(id, err.Error())
		return
	}
	statuses, err := c.run.Status(ctx, dir, game)
	if err != nil {
		c.fail(id, err.Error())
		return
	}
	if problem := gogVerificationProblem(statuses); problem != "" {
		c.fail(id, problem)
		return
	}

	// lgogdownloader writes a game under its %gamename% subdirectory; the
	// transfer's path is that directory, the bundle ingest walks.
	path := filepath.Join(dir, game)
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		path = dir
	}
	// Leave what GOG vouched for beside the package, so ingest can refuse the
	// transfer if any of it changed before it ran (acquisition.StoreManifestName).
	if err := writeStoreManifest(path, statuses); err != nil {
		c.fail(id, "writing the verified manifest: "+err.Error())
		return
	}
	c.mu.Lock()
	if t, ok := c.transfers[id]; ok {
		t.Done = true
		t.Path = path
	}
	c.mu.Unlock()
}

// gogVerificationProblem is "" when GOG vouched for every file, and otherwise a
// count of what it did not vouch for. No files at all is a problem too: a
// package GOG listed nothing for is not a package that was verified.
func gogVerificationProblem(statuses []GOGFileStatus) string {
	if len(statuses) == 0 {
		return "GOG reported no installer files for this game; nothing was verified"
	}
	bad := map[string]int{}
	for _, s := range statuses {
		if s.Code != "OK" {
			bad[s.Code]++
		}
	}
	if len(bad) == 0 {
		return ""
	}
	codes := make([]string, 0, len(bad))
	for code := range bad {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	parts := make([]string, 0, len(codes))
	for _, code := range codes {
		parts = append(parts, fmt.Sprintf("%s=%d", code, bad[code]))
	}
	return fmt.Sprintf("GOG verification failed for %d of %d files (%s)",
		len(statuses)-countOK(statuses), len(statuses), strings.Join(parts, ", "))
}

func writeStoreManifest(dir string, statuses []GOGFileStatus) error {
	m := acquisition.StoreManifest{Store: "gog"}
	for _, s := range statuses {
		m.Files = append(m.Files, acquisition.StoreManifestFile{Name: s.File, Size: s.Size})
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, acquisition.StoreManifestName), raw, 0o600)
}

func countOK(statuses []GOGFileStatus) int {
	n := 0
	for _, s := range statuses {
		if s.Code == "OK" {
			n++
		}
	}
	return n
}

func (c *GOGClient) fail(id, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.transfers[id]; ok {
		t.Error = string(TroubleClientError) + ": " + detail
	}
}

// Transfers implements providers.Downloader.
func (c *GOGClient) Transfers(_ context.Context) ([]providers.Transfer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]providers.Transfer, 0, len(c.transfers))
	for _, t := range c.transfers {
		out = append(out, *t)
	}
	sortTransfers(out)
	return out, nil
}

// Remove drops a transfer, refusing one it does not hold.
func (c *GOGClient) Remove(_ context.Context, id string, deleteData bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.transfers[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotOurs, id)
	}
	if deleteData && t.Path != "" {
		if err := os.RemoveAll(t.Path); err != nil {
			return err
		}
	}
	delete(c.transfers, id)
	return nil
}

// Label reports what this client tags its transfers with.
func (c *GOGClient) Label() string { return c.label }

// Dir is where packages are written.
func (c *GOGClient) Dir() string { return c.dir }

// execLgog is the production GOGRunner. Per ADR-0026 it is never exercised in
// CI; what is proven here is the invocation.
type execLgog struct{}

// lgogFilter restricts both the download and the status check to one game's
// installers (base game and DLC). The game name is validated by
// acquisition.ParseGOGSource before it gets here, so the anchored expression
// matches that one game and nothing it could be widened to.
func lgogFilter(dir, game string) []string {
	return []string{
		"--game", "^" + game + "$",
		"--directory", dir,
		"--include", "i",
		"--no-color", "--no-unicode",
	}
}

// Download runs `lgogdownloader --download` for one game.
func (execLgog) Download(ctx context.Context, dir, game string) error {
	if _, err := exec.LookPath(lgogBinary); err != nil {
		return fmt.Errorf("%s not found on PATH (install it and log in as the service user)", lgogBinary)
	}
	//nolint:gosec // G204: every argument is a constant or a validated gamename / heyarr-derived dir.
	cmd := exec.CommandContext(ctx, lgogBinary, append([]string{"--download"}, lgogFilter(dir, game)...)...)
	if err := cmd.Run(); err != nil {
		return errors.New("lgogdownloader failed to download the game (is the service user logged in?)")
	}
	return nil
}

// Status runs `lgogdownloader --status` for one game with the full MD5 check —
// the fast check compares sizes only, which would let a corrupt file through.
func (execLgog) Status(ctx context.Context, dir, game string) ([]GOGFileStatus, error) {
	if _, err := exec.LookPath(lgogBinary); err != nil {
		return nil, fmt.Errorf("%s not found on PATH", lgogBinary)
	}
	args := append([]string{"--status", "--no-fast-status-check"}, lgogFilter(dir, game)...)
	//nolint:gosec // G204: every argument is a constant or a validated gamename / heyarr-derived dir.
	out, err := exec.CommandContext(ctx, lgogBinary, args...).Output()
	if err != nil {
		return nil, errors.New("lgogdownloader could not report the game's file status")
	}
	return ParseGOGStatus(string(out)), nil
}

// gogStatusCodes are the codes `--status` documents. A line starting with
// anything else is progress or chatter, not a file.
var gogStatusCodes = map[string]bool{"OK": true, "ND": true, "MD5": true, "FS": true}

// ParseGOGStatus reads `lgogdownloader --status` output: one
// "code gamename filename size hash" line per file.
func ParseGOGStatus(out string) []GOGFileStatus {
	var statuses []GOGFileStatus
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || !gogStatusCodes[fields[0]] {
			continue
		}
		st := GOGFileStatus{Code: fields[0], File: fields[2]}
		if len(fields) > 3 {
			st.Size, _ = strconv.ParseInt(fields[3], 10, 64)
		}
		statuses = append(statuses, st)
	}
	return statuses
}
