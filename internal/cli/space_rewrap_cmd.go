package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rarebit-one/void-which-binds-go/encryption"
	"github.com/rarebit-one/void-which-binds-go/hashing"
	"github.com/rarebit-one/void-which-binds-go/migrate/gen1"
	"github.com/rarebit-one/void-which-binds-go/recovery"
	"github.com/rarebit-one/void-which-binds-go/useridentity"
	"github.com/spf13/cobra"

	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/custody"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spacerecover"
)

// `heyarr space rewrap` is the gen1-to-gen2 cutover's space-key rewrap
// (void-which-binds-go ADR-0022, C2 steps 4 to 7). It moves every space key from
// the gen1 recovery blob, exported by the pre-cutover binary from the stopped
// controller, to the gen2 laptop device and the gen2 recovery key. The space
// keys themselves are unchanged and no content is re-encrypted; only who the
// keys are sealed to changes. Gen1 material is read only through
// void-which-binds-go's read-only migrate/gen1 package.

// The stage directory's format and files.
const (
	rewrapStageFormat  = "heyarr-rewrap-stage-v1"
	rewrapManifestFile = "manifest.json"
	rewrapBlobFile     = "recovery.blob"
	rewrapSumsFile     = "SHA256SUMS"
	// rewrapMACFile (STAGE-MAC) is in space_rewrap_mac.go.
)

// rewrapManifest is a stage directory's manifest.json. It holds only public
// keys and wrapped (sealed) space keys, never a secret or a plaintext key.
type rewrapManifest struct {
	Format          string `json:"format"`
	Gen1User        string `json:"gen1_user"`
	Gen1Recovery    string `json:"gen1_recovery"`
	Gen2User        string `json:"gen2_user"`
	Gen2Recovery    string `json:"gen2_recovery"`
	DeviceRecipient string `json:"device_recipient"`
	BlobGeneratedAt string `json:"blob_generated_at"`
	// Spaces are sorted by space_id. Each carries exactly two wraps, keyed by
	// recipient: DeviceRecipient and Gen2Recovery, base64 (standard).
	Spaces []rewrapManifestSpace `json:"spaces"`
	// ExpectDBSpaceIDsSHA256 is the sha256 (hex) of the space ids of the
	// --expect-db copy's encrypted_spaces, sorted, each followed by "\n". It
	// equals the same digest over Spaces, because staging refuses any
	// difference between the two sets.
	ExpectDBSpaceIDsSHA256 string `json:"expect_db_space_ids_sha256"`
	// StageID is 128 random bits (32 lowercase hex digits) drawn by --stage
	// and printed to the operator, who passes it back as --stage-id to --prove
	// and --upload. The MAC covers it, so an older stage made with the same
	// gen2 secret cannot be replayed in place of this run's.
	StageID string `json:"stage_id"`
	// Gen1BlobBLAKE3 is the BLAKE3 digest (hex, unprefixed, as b3sum prints
	// it) of the gen1 recovery blob the stage was made from, for the operator
	// to check against the C2 step 4a export. BLAKE3, because bytes are
	// identified by their BLAKE3 digest (invariant 1).
	Gen1BlobBLAKE3 string `json:"gen1_blob_blake3"`
}

// rewrapStageIDBytes is the stage id's length: 128 bits.
const rewrapStageIDBytes = 16

// newRewrapStageID draws a fresh stage id.
func newRewrapStageID() (string, error) {
	b := make([]byte, rewrapStageIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("drawing the stage id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// parseRewrapStageID decodes a stage id: exactly 32 hex digits. The manifest
// writes it in lowercase; the operator may type either case.
func parseRewrapStageID(what, s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != rewrapStageIDBytes {
		return nil, fmt.Errorf("%s: a stage id is %d hex digits, got %q", what, 2*rewrapStageIDBytes, s)
	}
	return b, nil
}

// checkRewrapStageID refuses a stage whose (authenticated) manifest id is not
// the one the operator recorded from --stage, comparing in constant time.
func checkRewrapStageID(m rewrapManifest, want string) error {
	if m.StageID != strings.ToLower(m.StageID) {
		return fmt.Errorf("%s: stage_id %q is not lowercase hex", rewrapManifestFile, m.StageID)
	}
	got, err := parseRewrapStageID(rewrapManifestFile+": stage_id", m.StageID)
	if err != nil {
		return err
	}
	w, err := parseRewrapStageID("--stage-id", want)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(got, w) != 1 {
		return fmt.Errorf("this stage's id is %s, not --stage-id %s: it is not the stage this run wrote "+
			"(an older stage, or another run's); nothing was trusted from it", m.StageID, strings.ToLower(want))
	}
	return nil
}

type rewrapManifestSpace struct {
	SpaceID string            `json:"space_id"`
	Kind    string            `json:"kind"`
	Wraps   map[string]string `json:"wraps"`
}

// spaceRewrapView is the --json shape of every `space rewrap` mode. Like the
// recover result it carries no key material.
type spaceRewrapView struct {
	// Mode is "stage", "prove" or "upload".
	Mode  string `json:"mode"`
	Stage string `json:"stage"`
	// Proof is "full": the stage's MAC verifies under the gen2 secret, and the
	// gen2 secret alone, the device, and the recovery blob all open every space
	// to the same key. Every mode proves the stage in full.
	Proof string `json:"proof"`
	// StageID is the stage's id, which --prove and --upload take as --stage-id.
	StageID string `json:"stage_id"`
	// Gen1BlobBLAKE3 is the BLAKE3 (hex) of the gen1 blob the stage was made from.
	Gen1BlobBLAKE3  string                 `json:"gen1_blob_blake3"`
	Gen1User        string                 `json:"gen1_user"`
	Gen1Recovery    string                 `json:"gen1_recovery"`
	Gen2User        string                 `json:"gen2_user"`
	Gen2Recovery    string                 `json:"gen2_recovery"`
	DeviceRecipient string                 `json:"device_recipient"`
	BlobGeneratedAt string                 `json:"blob_generated_at"`
	Spaces          []spaceRewrapSpaceView `json:"spaces"`
}

type spaceRewrapSpaceView struct {
	SpaceID string `json:"space_id"`
	Kind    string `json:"kind"`
	// Proved is "ok" for every space; a space that fails is an error, not a row.
	Proved string `json:"proved"`
	// Upload is "uploaded", "uploaded-empty" (no content to check the key
	// against; the stage MAC vouches for it) or "already-present" in upload
	// mode.
	Upload string `json:"upload,omitempty"`
}

func newSpaceRewrapCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags       clientFlags
		from        string
		stage       string
		expectDB    string
		expect      string
		prove       string
		upload      string
		gen1File    string
		gen2File    string
		identityDir string
		stageID     string
	)
	cmd := &cobra.Command{
		Use:   "rewrap (--from <gen1.blob> --stage <dir> | --prove <dir> --stage-id <id> | --upload <dir> --stage-id <id>) --gen2-secret-file <f>",
		Short: "Rewrap every space key from the gen1 recovery blob to gen2 (void-which-binds ADR-0022 cutover)",
		Long: `Move every space key from gen1 (Voidbind) to gen2 (Void-Which-Binds) during the
cutover (void-which-binds-go ADR-0022, C2 steps 4 to 7). The space keys do not
change and no content is re-encrypted: each key is sealed afresh to the gen2
laptop device and the gen2 recovery key. Three modes, one at a time:

--from <gen1.blob> --stage <dir> --expect-db <db copy> [--expect <export.json>]
    OFFLINE, no server. Opens the gen1 heyarr-recovery-blob-v1 written by the
    pre-cutover ` + "`heyarr space export-recovery`" + ` with the gen1 secret, through
    void-which-binds-go's read-only migrate/gen1 package. Seals each key to this
    machine's device key (the configured custody backend; it must unwrap here,
    offline) and to the gen2 recovery key, and writes <dir>, which must not
    exist: manifest.json, recovery.blob (a gen2 recovery blob), SHA256SUMS and
    STAGE-MAC. The staged spaces must be EXACTLY the rows of encrypted_spaces
    in --expect-db (a copy of the frozen controller's backup, opened
    read-only), and exactly the space_ids of --expect when given. Any missing
    or extra space is a hard stop and nothing is written. It then proves the
    stage, and prints its stage id (128 random bits, new for every stage) and
    the BLAKE3 digest of the gen1 blob. Record the stage id, and check the
    digest against ` + "`b3sum gen1.blob`" + ` run where the export wrote it.

--prove <dir> --stage-id <id>
    Checks STAGE-MAC under the gen2 secret, checks the stage's id is --stage-id,
    opens every staged wrap with the
    gen2 secret alone and with this device, opens recovery.blob with the gen2
    secret, and checks all three give the same key for every space and that
    SHA256SUMS match.

--upload <dir> --stage-id <id>
    ONLINE, as the enrolled gen2 device. Proves the stage as --prove does,
    checks the controller holds exactly the staged spaces, checks each key
    opens the space's newest content (its latest snapshot, else its newest
    change; a space with no content is reported uploaded-empty), then uploads
    the device and gen2 recovery wraps and reads them back. Re-running it is
    safe. It never deletes a wrap, gen1 ones included. The controller accepts
    the recovery wrap only once ` + "`heyarr admin user rekey`" + ` has pinned the
    gen2 recovery key.

STAGE-MAC is an HMAC-SHA256 over the other three files, keyed from the gen2
secret, so every mode needs --gen2-secret-file: a stage that was changed after
--stage, by anyone without the gen2 secret, is refused before anything in it is
used. The MAC covers the stage id, and --prove and --upload refuse a stage
whose id is not --stage-id, the one this run's --stage printed: an older stage
made with the same gen2 secret (one from before a space key was rotated, say)
cannot stand in for it. Together they make a space with no content safe to
upload, with nothing on the controller to check its key against.

Secrets are read from FILES only, never argv or a prompt: --gen1-secret-file
and --gen2-secret-file, either of which (not both) may be "-" for standard
input. Each holds the recovery secret, or its SLIP-39 shares one per line.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			changed := func(names ...string) []string {
				var set []string
				for _, n := range names {
					if cmd.Flags().Changed(n) {
						set = append(set, "--"+n)
					}
				}
				return set
			}
			if gen2File == "" {
				return errors.New("every mode needs --gen2-secret-file: the stage is authenticated with a MAC keyed from the gen2 secret")
			}
			switch {
			case from != "":
				if cmd.Flags().Changed("stage-id") {
					return errors.New("--from draws a new stage id; --stage-id is for --prove and --upload")
				}
				if stage == "" || expectDB == "" || gen1File == "" {
					return errors.New("--from needs --stage, --expect-db, --gen1-secret-file and --gen2-secret-file")
				}
				v, err := runRewrapStage(cmd, configPath, *deviceDir, identityDir, rewrapStageInput{
					from: from, stage: stage, expectDB: expectDB, expect: expect, gen1File: gen1File, gen2File: gen2File,
				})
				if err != nil {
					return err
				}
				return emitRewrap(cmd, flags.asJSON, v)
			case prove != "":
				if bad := changed("stage", "expect-db", "expect", "gen1-secret-file", "identity-dir"); len(bad) > 0 {
					return fmt.Errorf("--prove does not take %s", strings.Join(bad, ", "))
				}
				if stageID == "" {
					return errors.New("--prove needs --stage-id: the id --stage printed for this run")
				}
				v, err := runRewrapProve(cmd, configPath, *deviceDir, prove, gen2File, stageID)
				if err != nil {
					return err
				}
				return emitRewrap(cmd, flags.asJSON, v)
			case upload != "":
				if bad := changed("stage", "expect-db", "expect", "gen1-secret-file", "identity-dir"); len(bad) > 0 {
					return fmt.Errorf("--upload does not take %s", strings.Join(bad, ", "))
				}
				if stageID == "" {
					return errors.New("--upload needs --stage-id: the id --stage printed for this run")
				}
				// The secret is read before the client is made, so a bad one
				// fails without touching the network.
				secret, err := readGen2Secret(cmd, gen2File)
				if err != nil {
					return err
				}
				return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
					v, err := runRewrapUpload(ctx, c, configPath, *deviceDir, upload, secret, stageID)
					if err != nil {
						return err
					}
					return emitRewrap(cmd, flags.asJSON, v)
				})
			default:
				return errors.New("name a mode: --from <gen1.blob>, --prove <dir> or --upload <dir>")
			}
		},
	}
	flags.register(cmd)
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "stage from this gen1 recovery blob (offline)")
	f.StringVar(&stage, "stage", "", "with --from: the stage directory to create (must not exist)")
	f.StringVar(&expectDB, "expect-db", "", "with --from: a COPY of the frozen controller database; the staged spaces must equal its encrypted_spaces")
	f.StringVar(&expect, "expect", "", "with --from: the gen1 export's --json output; the staged spaces must equal its space_ids")
	f.StringVar(&prove, "prove", "", "prove this stage directory (offline)")
	f.StringVar(&stageID, "stage-id", "", "with --prove and --upload: the stage id --stage printed for this run; any other stage is refused")
	f.StringVar(&upload, "upload", "", "upload this stage directory's wraps as this enrolled device (online)")
	f.StringVar(&gen1File, "gen1-secret-file", "", `with --from: read the gen1 recovery secret or shares from this file ("-": standard input)`)
	f.StringVar(&gen2File, "gen2-secret-file", "", `every mode: read the gen2 recovery secret or shares from this file ("-": standard input); it keys the stage MAC`)
	f.StringVar(&identityDir, "identity-dir", "",
		"with --from: where your gen2 user identity lives; when one is there, its recovery key must be the gen2 secret's (default: your config directory; "+useridentity.EnvDir+" overrides)")
	cmd.MarkFlagsMutuallyExclusive("from", "prove", "upload")
	cmd.MarkFlagsOneRequired("from", "prove", "upload")
	return cmd
}

func emitRewrap(cmd *cobra.Command, asJSON bool, v spaceRewrapView) error {
	if asJSON {
		return emitJSON(cmd.OutOrStdout(), v)
	}
	printSpaceRewrap(cmd.OutOrStdout(), v)
	return nil
}

type rewrapStageInput struct {
	from, stage, expectDB, expect, gen1File, gen2File string
}

// runRewrapStage is --from: open the gen1 blob, check the space set, seal each
// key to the gen2 device and recovery key, write the stage to a temporary
// directory, prove it, and only then rename it into place.
func runRewrapStage(cmd *cobra.Command, configPath *string, deviceDir, identityDir string, in rewrapStageInput) (spaceRewrapView, error) {
	ctx := cmd.Context()
	if in.gen1File == "-" && in.gen2File == "-" {
		return spaceRewrapView{}, errors.New("only one of --gen1-secret-file and --gen2-secret-file may be \"-\" (standard input)")
	}
	if _, err := os.Lstat(in.stage); err == nil {
		return spaceRewrapView{}, fmt.Errorf("the stage directory %s already exists; a stage is written once, into a new directory", in.stage)
	} else if !errors.Is(err, os.ErrNotExist) {
		return spaceRewrapView{}, err
	}

	raw1, err := readRewrapSecret(cmd, in.gen1File, "gen1")
	if err != nil {
		return spaceRewrapView{}, err
	}
	g1, err := parseGen1Input(raw1)
	if err != nil {
		return spaceRewrapView{}, fmt.Errorf("the gen1 secret was not accepted: %w", err)
	}
	raw2, err := readRewrapSecret(cmd, in.gen2File, "gen2")
	if err != nil {
		return spaceRewrapView{}, err
	}
	g2, err := parseRecoveryInput(raw2)
	if err != nil {
		return spaceRewrapView{}, fmt.Errorf("the gen2 secret was not accepted: %w", err)
	}
	g2id, err := deriveRecoveryIdentity(g2)
	if err != nil {
		return spaceRewrapView{}, err
	}
	if err := checkLocalGen2Identity(identityDir, g2id); err != nil {
		return spaceRewrapView{}, err
	}

	data, err := os.ReadFile(in.from) // #nosec G304 -- the operator explicitly passed the gen1 blob to read
	if err != nil {
		return spaceRewrapView{}, fmt.Errorf("reading the gen1 recovery blob: %w", err)
	}
	gen1Sum := hashing.New()
	_, _ = gen1Sum.Write(data)
	blob, err := g1.OpenRecoveryBlob(data)
	if err != nil {
		return spaceRewrapView{}, fmt.Errorf("opening the gen1 recovery blob with the gen1 secret: %w", err)
	}
	// OpenRecoveryBlob already refuses a blob for another recovery key or user;
	// say so again here, where the cutover depends on it.
	if blob.RecoveryRecipient != g1.RecoveryRecipient() {
		return spaceRewrapView{}, fmt.Errorf("the gen1 blob is sealed to %s, not this gen1 secret's recovery key %s", blob.RecoveryRecipient, g1.RecoveryRecipient())
	}
	if blob.UserID != "" && blob.UserID != g1.UserID() {
		return spaceRewrapView{}, fmt.Errorf("the gen1 blob names user %s, not this gen1 secret's %s", blob.UserID, g1.UserID())
	}

	staged := make([]string, 0, len(blob.Spaces))
	for _, s := range blob.Spaces {
		staged = append(staged, s.SpaceID)
	}
	dbIDs, err := expectDBSpaceIDs(ctx, in.expectDB)
	if err != nil {
		return spaceRewrapView{}, err
	}
	if err := sameSpaceSet("--expect-db's encrypted_spaces", dbIDs, staged); err != nil {
		return spaceRewrapView{}, err
	}
	if in.expect != "" {
		expIDs, err := expectExportSpaceIDs(in.expect, g1.RecoveryRecipient())
		if err != nil {
			return spaceRewrapView{}, err
		}
		if err := sameSpaceSet("--expect's space_ids", expIDs, staged); err != nil {
			return spaceRewrapView{}, err
		}
	}

	// Authenticate every key against the frozen database, not the blob alone.
	// The blob is sealed to a PUBLIC key, so anyone could forge one with the
	// right space ids and keys of their choosing; the database's own gen1
	// recovery wrap for each space, opened with the gen1 secret, must give the
	// same key. A space with no gen1 recovery wrap in the database stops here.
	dbWraps, err := expectDBRecoveryWraps(ctx, in.expectDB, g1.RecoveryRecipient())
	if err != nil {
		return spaceRewrapView{}, err
	}
	for _, s := range blob.Spaces {
		w, ok := dbWraps[s.SpaceID]
		if !ok {
			return spaceRewrapView{}, fmt.Errorf("space %s: --expect-db holds no wrap for the gen1 recovery key %s, so its key cannot be checked; nothing was staged", s.SpaceID, g1.RecoveryRecipient())
		}
		dbKey, err := g1.UnwrapSpaceKey(w)
		if err != nil {
			return spaceRewrapView{}, fmt.Errorf("space %s: the gen1 secret does not open --expect-db's recovery wrap: %w; nothing was staged", s.SpaceID, err)
		}
		if !sameSpaceKey(dbKey, s.Key) {
			return spaceRewrapView{}, fmt.Errorf("space %s: the blob's key is not the key in --expect-db (a forged or foreign blob); nothing was staged", s.SpaceID)
		}
	}

	cust, err := offlineCustody(configPath, deviceDir)
	if err != nil {
		return spaceRewrapView{}, err
	}
	device := cust.RecipientID()
	if device == g2id.RecoveryKey {
		return spaceRewrapView{}, errors.New("the device recipient is the gen2 recovery key; the device needs its own key")
	}

	keys := make(map[string]encryption.SpaceKey, len(blob.Spaces))
	for _, s := range blob.Spaces {
		keys[s.SpaceID] = s.Key
	}
	devWraps, err := spacerecover.RewrapForDevice(keys, device)
	if err != nil {
		return spaceRewrapView{}, err
	}
	recWraps, err := spacerecover.RewrapForDevice(keys, g2id.RecoveryKey)
	if err != nil {
		return spaceRewrapView{}, err
	}

	stageID, err := newRewrapStageID()
	if err != nil {
		return spaceRewrapView{}, err
	}
	m := rewrapManifest{
		Format:                 rewrapStageFormat,
		Gen1User:               g1.UserID(),
		Gen1Recovery:           g1.RecoveryRecipient(),
		Gen2User:               g2id.UserID,
		Gen2Recovery:           g2id.RecoveryKey,
		DeviceRecipient:        device,
		BlobGeneratedAt:        blob.GeneratedAt.UTC().Format(time.RFC3339),
		ExpectDBSpaceIDsSHA256: spaceIDsDigest(dbIDs),
		StageID:                stageID,
		Gen1BlobBLAKE3:         gen1Sum.Sum().Hex(),
	}
	gen2Blob := spacerecover.Blob{
		UserID:            g2id.UserID,
		RecoveryRecipient: g2id.RecoveryKey,
		// The gen1 export's time: the gen2 blob holds exactly that snapshot.
		GeneratedAt: blob.GeneratedAt.UTC(),
	}
	for _, s := range blob.Spaces {
		m.Spaces = append(m.Spaces, rewrapManifestSpace{
			SpaceID: s.SpaceID,
			Kind:    s.Kind,
			Wraps: map[string]string{
				device:           base64.StdEncoding.EncodeToString(devWraps[s.SpaceID]),
				g2id.RecoveryKey: base64.StdEncoding.EncodeToString(recWraps[s.SpaceID]),
			},
		})
		gen2Blob.Spaces = append(gen2Blob.Spaces, spacerecover.BlobSpace{
			SpaceID: s.SpaceID, Kind: s.Kind, Wrapped: recWraps[s.SpaceID],
		})
	}
	sort.Slice(m.Spaces, func(i, j int) bool { return m.Spaces[i].SpaceID < m.Spaces[j].SpaceID })
	blobData, err := spacerecover.SealBlob(gen2Blob)
	if err != nil {
		return spaceRewrapView{}, err
	}

	return writeAndProveStage(in.stage, m, blobData, g2, cust)
}

// writeAndProveStage writes the stage to a temporary sibling directory, MACs it
// under the gen2 secret, proves it there, and renames it into place, so a
// failure leaves nothing behind.
func writeAndProveStage(stage string, m rewrapManifest, blobData []byte, g2 recovery.Secret, cust client.Custody) (spaceRewrapView, error) {
	manifestData, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return spaceRewrapView{}, err
	}
	manifestData = append(manifestData, '\n')

	clean := filepath.Clean(stage)
	// MkdirTemp creates the directory 0700, the stage's mode.
	tmp, err := os.MkdirTemp(filepath.Dir(clean), "."+filepath.Base(clean)+".partial-")
	if err != nil {
		return spaceRewrapView{}, fmt.Errorf("creating the stage: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = os.RemoveAll(tmp)
		}
	}()
	sums := fmt.Sprintf("%x  %s\n%x  %s\n",
		sha256.Sum256(manifestData), rewrapManifestFile, sha256.Sum256(blobData), rewrapBlobFile)
	covered := map[string][]byte{
		rewrapManifestFile: manifestData,
		rewrapBlobFile:     blobData,
		rewrapSumsFile:     []byte(sums),
	}
	tag, err := rewrapStageMAC(g2, covered)
	if err != nil {
		return spaceRewrapView{}, err
	}
	for _, name := range rewrapMACFiles {
		if err := writeNewFile(filepath.Join(tmp, name), covered[name]); err != nil {
			return spaceRewrapView{}, err
		}
	}
	if err := writeNewFile(filepath.Join(tmp, rewrapMACFile), formatStageMAC(tag)); err != nil {
		return spaceRewrapView{}, err
	}

	v, _, err := proveStage(tmp, g2, m.StageID, cust)
	if err != nil {
		return spaceRewrapView{}, fmt.Errorf("the freshly written stage did not prove, so nothing was kept: %w", err)
	}
	if _, err := os.Lstat(clean); err == nil {
		return spaceRewrapView{}, fmt.Errorf("the stage directory %s appeared while staging; nothing was written to it", stage)
	}
	if err := os.Rename(tmp, clean); err != nil {
		return spaceRewrapView{}, fmt.Errorf("moving the stage into place: %w", err)
	}
	done = true
	v.Mode, v.Stage = "stage", stage
	return v, nil
}

func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- inside the stage directory this command just created
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readGen2Secret reads and parses the gen2 recovery secret for --prove and
// --upload.
func readGen2Secret(cmd *cobra.Command, gen2File string) (recovery.Secret, error) {
	raw, err := readRewrapSecret(cmd, gen2File, "gen2")
	if err != nil {
		return recovery.Secret{}, err
	}
	s, err := parseRecoveryInput(raw)
	if err != nil {
		return recovery.Secret{}, fmt.Errorf("the gen2 secret was not accepted: %w", err)
	}
	return s, nil
}

// runRewrapProve is --prove: the stage MAC and the full proof, with the gen2
// secret.
func runRewrapProve(cmd *cobra.Command, configPath *string, deviceDir, dir, gen2File, stageID string) (spaceRewrapView, error) {
	secret, err := readGen2Secret(cmd, gen2File)
	if err != nil {
		return spaceRewrapView{}, err
	}
	cust, err := offlineCustody(configPath, deviceDir)
	if err != nil {
		return spaceRewrapView{}, err
	}
	v, _, err := proveStage(dir, secret, stageID, cust)
	if err != nil {
		return spaceRewrapView{}, err
	}
	v.Mode, v.Stage = "prove", dir
	return v, nil
}

// provedStage is a stage's content as proveStage read, authenticated and
// proved it. Upload works from it alone and never re-reads the directory, so
// the bytes it uploads are the bytes that were proved.
type provedStage struct {
	m     rewrapManifest
	wraps map[string]map[string][]byte
}

// proveStage is PROVE (ADR-0022 C2 step 5) over a stage directory. It first
// checks the directory holds exactly the stage files and that STAGE-MAC
// verifies under the secret, before anything in the stage is parsed; then that
// SHA256SUMS match, and that the stage's id is stageID, the one the operator
// recorded from --stage. Then the full proof: the secret derives the manifest's gen2
// user and recovery key, the recovery wraps open with the secret alone, the
// device wraps open through custody, recovery.blob opens with the secret, and
// all three give the same key for every space.
func proveStage(dir string, secret recovery.Secret, stageID string, cust client.Custody) (spaceRewrapView, provedStage, error) {
	m, blobData, err := readStage(dir, secret)
	if err != nil {
		return spaceRewrapView{}, provedStage{}, err
	}
	if err := checkRewrapStageID(m, stageID); err != nil {
		return spaceRewrapView{}, provedStage{}, err
	}
	wraps, err := decodeStageWraps(m)
	if err != nil {
		return spaceRewrapView{}, provedStage{}, err
	}

	if got := cust.RecipientID(); got != m.DeviceRecipient {
		return spaceRewrapView{}, provedStage{}, fmt.Errorf("this device's key is %s, but the stage was wrapped for %s; prove and upload from the device that staged it", got, m.DeviceRecipient)
	}
	devKeys := make(map[string]encryption.SpaceKey, len(m.Spaces))
	for _, s := range m.Spaces {
		k, err := cust.Unwrap(wraps[s.SpaceID][m.DeviceRecipient])
		if err != nil {
			return spaceRewrapView{}, provedStage{}, fmt.Errorf("space %s: this device does not open its staged wrap: %w", s.SpaceID, err)
		}
		devKeys[s.SpaceID] = k
	}
	if err := proveWithSecret(m, wraps, blobData, secret, devKeys); err != nil {
		return spaceRewrapView{}, provedStage{}, err
	}

	v := spaceRewrapView{
		Proof:           "full",
		StageID:         m.StageID,
		Gen1BlobBLAKE3:  m.Gen1BlobBLAKE3,
		Gen1User:        m.Gen1User,
		Gen1Recovery:    m.Gen1Recovery,
		Gen2User:        m.Gen2User,
		Gen2Recovery:    m.Gen2Recovery,
		DeviceRecipient: m.DeviceRecipient,
		BlobGeneratedAt: m.BlobGeneratedAt,
		Spaces:          make([]spaceRewrapSpaceView, 0, len(m.Spaces)),
	}
	for _, s := range m.Spaces {
		v.Spaces = append(v.Spaces, spaceRewrapSpaceView{SpaceID: s.SpaceID, Kind: s.Kind, Proved: "ok"})
	}
	return v, provedStage{m: m, wraps: wraps}, nil
}

func proveWithSecret(m rewrapManifest, wraps map[string]map[string][]byte, blobData []byte, secret recovery.Secret, devKeys map[string]encryption.SpaceKey) error {
	id, err := deriveRecoveryIdentity(secret)
	if err != nil {
		return err
	}
	if id.UserID != m.Gen2User || id.RecoveryKey != m.Gen2Recovery {
		return fmt.Errorf("the gen2 secret derives user %s and recovery key %s, but the stage is for %s and %s",
			id.UserID, id.RecoveryKey, m.Gen2User, m.Gen2Recovery)
	}
	recWraps := make(map[string][]byte, len(m.Spaces))
	for _, s := range m.Spaces {
		recWraps[s.SpaceID] = wraps[s.SpaceID][m.Gen2Recovery]
	}
	recKeys, err := spacerecover.UnwrapAll(secret, recWraps)
	if err != nil {
		return fmt.Errorf("the gen2 secret alone does not open every staged recovery wrap: %w", err)
	}
	blob, err := spacerecover.OpenBlob(secret, blobData)
	if err != nil {
		return fmt.Errorf("%s: %w", rewrapBlobFile, err)
	}
	if blob.UserID != m.Gen2User {
		return fmt.Errorf("%s names user %q, but the stage is for %s", rewrapBlobFile, blob.UserID, m.Gen2User)
	}
	if len(blob.Spaces) != len(m.Spaces) {
		return fmt.Errorf("%s holds %d space(s), the manifest %d", rewrapBlobFile, len(blob.Spaces), len(m.Spaces))
	}
	blobKeys, err := spacerecover.UnwrapAll(secret, blob.Wrapped())
	if err != nil {
		return fmt.Errorf("%s: %w", rewrapBlobFile, err)
	}
	kinds := make(map[string]string, len(blob.Spaces))
	for _, s := range blob.Spaces {
		kinds[s.SpaceID] = s.Kind
	}
	for _, s := range m.Spaces {
		bk, ok := blobKeys[s.SpaceID]
		if !ok || kinds[s.SpaceID] != s.Kind {
			return fmt.Errorf("space %s: %s does not hold it as the manifest does", s.SpaceID, rewrapBlobFile)
		}
		if !sameSpaceKey(recKeys[s.SpaceID], devKeys[s.SpaceID]) || !sameSpaceKey(recKeys[s.SpaceID], bk) {
			return fmt.Errorf("space %s: the recovery wrap, the device wrap and %s do not all hold the same key", s.SpaceID, rewrapBlobFile)
		}
	}
	return nil
}

// readStage checks the stage directory holds exactly its four files, that
// STAGE-MAC verifies under the gen2 secret over the other three (before any of
// them is parsed), that SHA256SUMS lists exactly the manifest and the blob and
// matches them, and parses the manifest strictly.
func readStage(dir string, secret recovery.Secret) (rewrapManifest, []byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return rewrapManifest{}, nil, fmt.Errorf("reading the stage directory: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			return rewrapManifest{}, nil, fmt.Errorf("the stage directory holds %s, which is not a regular file", e.Name())
		}
		names = append(names, e.Name())
	}
	want := append([]string{rewrapMACFile}, rewrapMACFiles...)
	sort.Strings(names)
	sort.Strings(want)
	if !slices.Equal(names, want) {
		return rewrapManifest{}, nil, fmt.Errorf("the stage directory holds %v, want exactly %v", names, want)
	}
	read := func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- a fixed name inside the stage directory the operator named
	}
	contents := make(map[string][]byte, len(rewrapMACFiles))
	for _, name := range rewrapMACFiles {
		data, err := read(name)
		if err != nil {
			return rewrapManifest{}, nil, err
		}
		contents[name] = data
	}
	macFile, err := read(rewrapMACFile)
	if err != nil {
		return rewrapManifest{}, nil, err
	}
	if err := verifyStageMAC(secret, macFile, contents); err != nil {
		return rewrapManifest{}, nil, err
	}

	sums := contents[rewrapSumsFile]
	listed := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		sum, name, ok := strings.Cut(sc.Text(), "  ")
		if !ok || listed[name] != "" {
			return rewrapManifest{}, nil, fmt.Errorf("%s: malformed line %q", rewrapSumsFile, sc.Text())
		}
		listed[name] = sum
	}
	if len(listed) != 2 || listed[rewrapManifestFile] == "" || listed[rewrapBlobFile] == "" {
		return rewrapManifest{}, nil, fmt.Errorf("%s must list exactly %s and %s", rewrapSumsFile, rewrapManifestFile, rewrapBlobFile)
	}
	for _, name := range []string{rewrapManifestFile, rewrapBlobFile} {
		got := sha256.Sum256(contents[name])
		if hex.EncodeToString(got[:]) != listed[name] {
			return rewrapManifest{}, nil, fmt.Errorf("%s does not match %s: the stage was changed or damaged", name, rewrapSumsFile)
		}
	}

	var m rewrapManifest
	dec := json.NewDecoder(bytes.NewReader(contents[rewrapManifestFile]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return rewrapManifest{}, nil, fmt.Errorf("%s: %w", rewrapManifestFile, err)
	}
	if m.Format != rewrapStageFormat {
		return rewrapManifest{}, nil, fmt.Errorf("%s: format %q, want %q", rewrapManifestFile, m.Format, rewrapStageFormat)
	}
	return m, contents[rewrapBlobFile], nil
}

// decodeStageWraps checks the manifest's shape (sorted unique spaces, each with
// exactly the device and gen2 recovery wraps, and the space-set digest) and
// decodes the wraps, keyed by space then recipient.
func decodeStageWraps(m rewrapManifest) (map[string]map[string][]byte, error) {
	for _, k := range []struct{ name, val string }{
		{"gen2_recovery", m.Gen2Recovery}, {"device_recipient", m.DeviceRecipient},
	} {
		if _, err := encryption.ParsePublicKey(k.val); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", rewrapManifestFile, k.name, err)
		}
	}
	if m.Gen2Recovery == m.DeviceRecipient {
		return nil, fmt.Errorf("%s: the device and recovery recipients are the same key", rewrapManifestFile)
	}
	if len(m.Spaces) == 0 {
		return nil, fmt.Errorf("%s: no spaces", rewrapManifestFile)
	}
	ids := make([]string, 0, len(m.Spaces))
	out := make(map[string]map[string][]byte, len(m.Spaces))
	for i, s := range m.Spaces {
		if s.SpaceID == "" || (i > 0 && s.SpaceID <= m.Spaces[i-1].SpaceID) {
			return nil, fmt.Errorf("%s: space ids must be non-empty, unique and sorted (at %q)", rewrapManifestFile, s.SpaceID)
		}
		if len(s.Wraps) != 2 || s.Wraps[m.DeviceRecipient] == "" || s.Wraps[m.Gen2Recovery] == "" {
			return nil, fmt.Errorf("%s: space %s must carry exactly the device and gen2 recovery wraps", rewrapManifestFile, s.SpaceID)
		}
		out[s.SpaceID] = map[string][]byte{}
		for r, b64 := range s.Wraps {
			w, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return nil, fmt.Errorf("%s: space %s, wrap for %s: %w", rewrapManifestFile, s.SpaceID, r, err)
			}
			out[s.SpaceID][r] = w
		}
		ids = append(ids, s.SpaceID)
	}
	if spaceIDsDigest(ids) != m.ExpectDBSpaceIDsSHA256 {
		return nil, fmt.Errorf("%s: the staged spaces are not the set the stage was checked against (expect_db_space_ids_sha256)", rewrapManifestFile)
	}
	return out, nil
}

// runRewrapUpload is --upload: the stage MAC, the stage id and the full proof, the
// server-side set and content checks for every space, then the uploads and
// their read-back.
func runRewrapUpload(ctx context.Context, c *apiclient.Client, configPath *string, deviceDir, dir string, secret recovery.Secret, stageID string) (spaceRewrapView, error) {
	cust, err := selectCustody(configPath, deviceDir)
	if err != nil {
		return spaceRewrapView{}, err
	}
	v, st, err := proveStage(dir, secret, stageID, cust)
	if err != nil {
		return spaceRewrapView{}, err
	}
	m, wraps := st.m, st.wraps

	list, err := c.ListSpaces(ctx)
	if err != nil {
		return spaceRewrapView{}, err
	}
	server := make([]string, 0, len(list))
	for _, sp := range list {
		server = append(server, sp.ID)
	}
	staged := make([]string, 0, len(m.Spaces))
	for _, s := range m.Spaces {
		staged = append(staged, s.SpaceID)
	}
	if err := sameSpaceSet("the controller's spaces", server, staged); err != nil {
		return spaceRewrapView{}, err
	}

	// Every key is checked against the controller's content before anything
	// is uploaded. A space with no content has nothing to check against, and
	// needs nothing: staging bound each key to the frozen database's own gen1
	// recovery wrap, and STAGE-MAC, which only the gen2 secret can make, has
	// just shown the stage is byte for byte what staging wrote (#692). Writes
	// are stopped from C2 step 3, so the key cannot have moved since.
	empty := make(map[string]bool)
	for _, s := range m.Spaces {
		k, err := cust.Unwrap(wraps[s.SpaceID][m.DeviceRecipient])
		if err != nil {
			return spaceRewrapView{}, fmt.Errorf("space %s: %w", s.SpaceID, err)
		}
		isEmpty, err := verifyRemoteSpaceKey(ctx, c, s.SpaceID, k)
		if err != nil {
			return spaceRewrapView{}, err
		}
		empty[s.SpaceID] = isEmpty
	}

	recipients := []string{m.DeviceRecipient, m.Gen2Recovery}
	for i, s := range m.Spaces {
		present, err := hasStagedWraps(ctx, c, s.SpaceID, wraps[s.SpaceID], recipients)
		if err != nil {
			return spaceRewrapView{}, err
		}
		if present {
			v.Spaces[i].Upload = "already-present"
			continue
		}
		inputs := make([]apiclient.WrappedKeyInput, 0, len(recipients))
		for _, r := range recipients {
			inputs = append(inputs, apiclient.WrappedKeyInput{Recipient: r, Wrapped: wraps[s.SpaceID][r]})
		}
		if err := c.RewrapKeys(ctx, s.SpaceID, inputs); err != nil {
			var apiErr *apiclient.Error
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
				return spaceRewrapView{}, fmt.Errorf("space %s: %w\n"+
					"the controller does not yet accept the gen2 wrap recipients: pin the gen2 recovery key with "+
					"`heyarr admin user rekey <principal> %s --recovery-key %s` and enrol this device (%s), then re-run --upload",
					s.SpaceID, err, m.Gen2User, m.Gen2Recovery, m.DeviceRecipient)
			}
			return spaceRewrapView{}, fmt.Errorf("space %s: uploading the wraps: %w", s.SpaceID, err)
		}
		present, err = hasStagedWraps(ctx, c, s.SpaceID, wraps[s.SpaceID], recipients)
		if err != nil {
			return spaceRewrapView{}, err
		}
		if !present {
			return spaceRewrapView{}, fmt.Errorf("space %s: the controller accepted the wraps but does not return them", s.SpaceID)
		}
		v.Spaces[i].Upload = "uploaded"
		if empty[s.SpaceID] {
			v.Spaces[i].Upload = "uploaded-empty"
		}
	}
	v.Mode, v.Stage = "upload", dir
	return v, nil
}

// hasStagedWraps reports whether the controller holds, for every recipient,
// exactly the staged wrap.
func hasStagedWraps(ctx context.Context, c *apiclient.Client, spaceID string, staged map[string][]byte, recipients []string) (bool, error) {
	keys, err := c.WrappedKeys(ctx, spaceID)
	if err != nil {
		return false, fmt.Errorf("space %s: reading its wrapped keys: %w", spaceID, err)
	}
	held := make(map[string][]byte, len(keys))
	for _, k := range keys {
		held[k.Recipient] = k.Wrapped
	}
	for _, r := range recipients {
		if !bytes.Equal(held[r], staged[r]) {
			return false, nil
		}
	}
	return true, nil
}

// verifyRemoteSpaceKey checks the staged key against the space's NEWEST
// content on the controller, in causal order. With a snapshot, the key must
// open the snapshot and every change past its frontier (not the frontier or
// one of its ancestors): a change appended after a rotation whose snapshot
// never landed is under the newer key, so a stale stage is refused. The
// frontier is used only when the snapshot's authenticated envelope vouches for
// it. Otherwise (no snapshot, or a legacy one), the key must open every head change (one no other change names as
// a parent). A space with no content reports empty: staging bound the stage's
// keys to the frozen database, STAGE-MAC carries that binding to the upload,
// and writes are stopped from C2 step 3.
func verifyRemoteSpaceKey(ctx context.Context, c *apiclient.Client, spaceID string, k encryption.SpaceKey) (empty bool, err error) {
	snap, hasSnap, err := c.Snapshot(ctx, spaceID)
	if err != nil {
		return false, fmt.Errorf("space %s: reading its latest snapshot: %w", spaceID, err)
	}
	changes, err := c.Changes(ctx, spaceID)
	if err != nil {
		return false, fmt.Errorf("space %s: reading its changes: %w", spaceID, err)
	}
	if !hasSnap && len(changes) == 0 {
		return true, nil
	}
	stale := func() error {
		return fmt.Errorf("space %s: the staged key does not open the space's newest content on the controller "+
			"(the space was re-keyed after the gen1 export, or the blob was not this controller's); nothing was uploaded", spaceID)
	}
	parents := make(map[string][]string, len(changes))
	for _, ch := range changes {
		parents[ch.ChangeID] = ch.Parents
	}
	var newest []protocol.EncryptedChange
	// The snapshot's frontier is public and only trustworthy once its
	// authenticated envelope (inside the ciphertext) is checked against it: a
	// relabelled frontier would otherwise hide newer changes from the scan. A
	// mismatched envelope is refused; a legacy snapshot with no envelope proves
	// nothing about its frontier, so every head change is checked instead.
	trustFrontier := false
	if hasSnap {
		pt, err := encryption.DecryptChange(k, snap.Ciphertext)
		if err != nil {
			return false, stale()
		}
		_, authenticated, err := protocol.OpenSnapshotPlaintext(snap, pt)
		if err != nil {
			return false, fmt.Errorf("space %s: the controller's latest snapshot does not match its own envelope (%w); nothing was uploaded", spaceID, err)
		}
		trustFrontier = authenticated
	}
	if trustFrontier {
		covered := make(map[string]bool)
		stack := append([]string(nil), snap.Frontier...)
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if covered[id] {
				continue
			}
			covered[id] = true
			stack = append(stack, parents[id]...)
		}
		for _, ch := range changes {
			if !covered[ch.ChangeID] {
				newest = append(newest, ch)
			}
		}
	} else {
		named := make(map[string]bool)
		for _, ch := range changes {
			for _, p := range ch.Parents {
				named[p] = true
			}
		}
		for _, ch := range changes {
			if !named[ch.ChangeID] {
				newest = append(newest, ch)
			}
		}
	}
	for _, ch := range newest {
		if _, err := encryption.DecryptChange(k, ch.Ciphertext); err != nil {
			return false, stale()
		}
	}
	return false, nil
}

// readRewrapSecret reads a secret from a file, or from standard input for "-".
func readRewrapSecret(cmd *cobra.Command, path, which string) (string, error) {
	var (
		raw []byte
		err error
	)
	if path == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
	} else {
		raw, err = os.ReadFile(path) // #nosec G304 -- the operator explicitly passed this path to read their own secret
	}
	if err != nil {
		return "", fmt.Errorf("reading the %s secret: %w", which, err)
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", fmt.Errorf("the %s secret file is empty", which)
	}
	return s, nil
}

// parseGen1Input is parseRecoveryInput for gen1: the "heyarr1…" secret, or at
// least minShareWords words per line read as SLIP-39 shares (no passphrase, as
// for gen2). Shares carry no generation, so a wrong set is caught when the
// blob does not open.
func parseGen1Input(raw string) (gen1.Secret, error) {
	text := strings.TrimSpace(raw)
	secret, err := gen1.ParseSecret(text)
	if err == nil {
		return secret, nil
	}
	firstLine, _, _ := strings.Cut(text, "\n")
	if len(strings.Fields(firstLine)) < minShareWords {
		return gen1.Secret{}, err
	}
	var shares []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			shares = append(shares, line)
		}
	}
	secret, err = gen1.CombineShares(shares, nil)
	if err != nil {
		return gen1.Secret{}, fmt.Errorf("the gen1 recovery shares did not combine: %w", err)
	}
	return secret, nil
}

// checkLocalGen2Identity refuses a gen2 secret that is not the local gen2 user
// identity's, when this machine has one.
func checkLocalGen2Identity(identityDir string, g2 recoveryIdentity) error {
	store, err := openUserIdentityStore(identityDir)
	if err != nil {
		return err
	}
	id, err := store.Get()
	if errors.Is(err, useridentity.ErrNoIdentity) {
		return nil
	}
	if err != nil {
		return err
	}
	if id.EncryptionKey != g2.RecoveryKey {
		return fmt.Errorf("the gen2 secret's recovery key %s is not this machine's gen2 identity's %s", g2.RecoveryKey, id.EncryptionKey)
	}
	return nil
}

// offlineCustody is selectCustody for the offline modes, which need a backend
// that unwraps on this machine. The cruciform backend unwraps on a paired
// phone over the relay, so it is refused.
func offlineCustody(configPath *string, deviceDir string) (client.Custody, error) {
	path := ""
	if configPath != nil {
		path = *configPath
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if cfg.Vault.Unwrapper == custody.Cruciform {
		return nil, fmt.Errorf("vault.unwrapper is %q, which unwraps on a paired phone over the network; "+
			"the rewrap is offline and needs a device key that unwraps here (the gen2 laptop device's software key)", custody.Cruciform)
	}
	return selectCustody(configPath, deviceDir)
}

// expectDBSpaceIDs reads every encrypted_spaces id from a copy of the frozen
// controller database, opened read-only.
func expectDBSpaceIDs(ctx context.Context, path string) ([]string, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("--expect-db: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(true)")
	if err != nil {
		return nil, fmt.Errorf("--expect-db: opening read-only: %w", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, `SELECT id FROM encrypted_spaces ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("--expect-db: reading encrypted_spaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("--expect-db: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("--expect-db: %w", err)
	}
	return ids, nil
}

// expectDBRecoveryWraps reads every space's wrap for the gen1 recovery
// recipient from the read-only copy of the frozen controller database.
func expectDBRecoveryWraps(ctx context.Context, path, recipient string) (map[string][]byte, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(true)")
	if err != nil {
		return nil, fmt.Errorf("--expect-db: opening read-only: %w", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, `SELECT space_id, wrapped FROM wrapped_keys WHERE recipient = ?`, recipient)
	if err != nil {
		return nil, fmt.Errorf("--expect-db: reading wrapped_keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string][]byte)
	for rows.Next() {
		var id string
		var w []byte
		if err := rows.Scan(&id, &w); err != nil {
			return nil, fmt.Errorf("--expect-db: %w", err)
		}
		out[id] = w
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("--expect-db: %w", err)
	}
	return out, nil
}

// expectExportSpaceIDs reads the space_ids of a `space export-recovery --json`
// output, checking it was exported for the gen1 recovery key.
func expectExportSpaceIDs(path, gen1Recovery string) ([]string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the operator explicitly passed the export's JSON
	if err != nil {
		return nil, fmt.Errorf("--expect: %w", err)
	}
	var v spaceExportView
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("--expect: not an export-recovery --json output: %w", err)
	}
	if v.SpaceIDs == nil {
		return nil, errors.New("--expect: no space_ids in it")
	}
	if v.Recipient != "" && v.Recipient != gen1Recovery {
		return nil, fmt.Errorf("--expect: exported for recovery key %s, not the gen1 secret's %s", v.Recipient, gen1Recovery)
	}
	return v.SpaceIDs, nil
}

// sameSpaceSet is the cutover's hard stop: the staged spaces must be exactly
// the reference set, and any difference names every id on each side.
func sameSpaceSet(what string, reference, staged []string) error {
	ref := make(map[string]bool, len(reference))
	for _, id := range reference {
		ref[id] = true
	}
	st := make(map[string]bool, len(staged))
	for _, id := range staged {
		st[id] = true
	}
	var missing, extra []string
	for id := range ref {
		if !st[id] {
			missing = append(missing, id)
		}
	}
	for id := range st {
		if !ref[id] {
			extra = append(extra, id)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	sort.Strings(missing)
	sort.Strings(extra)
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("in %s but not staged: %s", what, strings.Join(missing, ", ")))
	}
	if len(extra) > 0 {
		parts = append(parts, fmt.Sprintf("staged but not in %s: %s", what, strings.Join(extra, ", ")))
	}
	return fmt.Errorf("the staged spaces are not exactly %s (%s); nothing was written. "+
		"Stop the cutover here (ADR-0022 C2 step 5)", what, strings.Join(parts, "; "))
}

// spaceIDsDigest is the sha256 (hex) of the ids, sorted, each followed by "\n".
func spaceIDsDigest(ids []string) string {
	sorted := slices.Clone(ids)
	sort.Strings(sorted)
	h := sha256.New()
	for _, id := range sorted {
		_, _ = io.WriteString(h, id+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sameSpaceKey reports whether a and b are the same key: SpaceKey does not
// expose its bytes, so a probe sealed under a must open under b. The AEAD makes
// a false match infeasible.
func sameSpaceKey(a, b encryption.SpaceKey) bool {
	ct, err := encryption.EncryptChange(a, []byte("heyarr space rewrap key probe"))
	if err != nil {
		return false
	}
	_, err = encryption.DecryptChange(b, ct)
	return err == nil
}

func printSpaceRewrap(w io.Writer, v spaceRewrapView) {
	switch v.Mode {
	case "stage":
		fmt.Fprintf(w, "Staged %d space(s) in %s from the gen1 blob exported %s.\n", len(v.Spaces), v.Stage, v.BlobGeneratedAt)
	case "prove":
		fmt.Fprintf(w, "Proved the stage %s (%s).\n", v.Stage, v.Proof)
	case "upload":
		fmt.Fprintf(w, "Uploaded the stage %s.\n", v.Stage)
	}
	fmt.Fprintf(w, "  stage id %s\n", v.StageID)
	fmt.Fprintf(w, "  gen1 blob blake3 %s\n", v.Gen1BlobBLAKE3)
	fmt.Fprintf(w, "  gen1 %s (recovery %s)\n", v.Gen1User, v.Gen1Recovery)
	fmt.Fprintf(w, "  gen2 %s (recovery %s)\n", v.Gen2User, v.Gen2Recovery)
	fmt.Fprintf(w, "  device %s\n", v.DeviceRecipient)
	for _, s := range v.Spaces {
		line := fmt.Sprintf("  %s  %-8s  %s", s.SpaceID, s.Kind, strings.ToUpper(s.Proved))
		if s.Upload != "" {
			line += "  " + s.Upload
		}
		fmt.Fprintln(w, line)
	}
	if v.Mode == "stage" {
		fmt.Fprintln(w, "Record the stage id: --prove and --upload take it as --stage-id. Check the gen1 blob blake3 against `b3sum` of the exported blob.")
	}
}
