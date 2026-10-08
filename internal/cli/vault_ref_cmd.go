package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaceopen"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultframe"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultread"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultref"
)

// The exit codes of the ref-addressed vault commands. A caller that shells out
// (an executor's runner) maps them to its own error codes without parsing
// prose; anything not listed is 1.
const (
	// ExitVaultCustody: the key that opens spaces is unavailable — the sealed
	// file is missing, the PIN source is absent, or the PIN is wrong.
	ExitVaultCustody = 3
	// ExitVaultForbidden: the space is not visible to this credential — no
	// grant, a revoked or expired one, or no such space. The server answers
	// all of these alike (ADR-0104).
	ExitVaultForbidden = 4
	// ExitVaultUnwrap: the space is visible but cannot be decrypted — no copy
	// of its key is wrapped for this key, or the copy does not open.
	ExitVaultUnwrap = 5
	// ExitVaultAbsent: the ref names no object, or one with conflicting
	// versions.
	ExitVaultAbsent = 6
	// ExitVaultIntegrity: the node served bytes that are not the object the
	// ref names — a manifest or content blob that does not hash to its id, or
	// a frame that is not this file's (vaultread.ErrBlobIntegrity), or a
	// manifest whose geometry is impossible (vaultframe.ErrManifest). Something
	// was served, and it was wrong: not an absent object, and not one to
	// retry as if it were.
	ExitVaultIntegrity = 7
)

// exitError is an error that ends the process with a specific exit code.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// ExitCode is the process exit status Main uses for this error.
func (e *exitError) ExitCode() int { return e.code }

func withExit(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: code, err: err}
}

// maxVaultObject bounds a sealed object put by ref: a question, an answer or
// an index entry, never a document (documents are pushed by path).
const maxVaultObject = 8 << 20

// vaultObjectEnvelope is the part of a sealed object's JSON put-ref checks: a
// versioned, typed envelope. The rest is the caller's.
type vaultObjectEnvelope struct {
	V    *int   `json:"v"`
	Type string `json:"type"`
}

// classifyOpenedRead maps a read failure AFTER the space opened. Access is
// settled by then, so a 404 means the drive entry names a manifest or content
// blob this node cannot serve (replication lag, storage loss): an absent
// object, not a missing grant. Bytes that do not match their content address
// are an integrity failure, checked first so a wrapped one is never read as
// anything else. A 401/403 still means access went away.
func classifyOpenedRead(ref string, err error) error {
	switch {
	case errors.Is(err, vaultread.ErrBlobIntegrity), errors.Is(err, vaultframe.ErrManifest):
		return withExit(ExitVaultIntegrity, fmt.Errorf("%s: %w", ref, err))
	case errors.Is(err, errVaultPathAbsent), errors.Is(err, errVaultPathConflicted), isNotFound(err):
		return withExit(ExitVaultAbsent, fmt.Errorf("%s: %w", ref, err))
	default:
		return classifyAPI(err)
	}
}

// isNotFound reports whether err is the API answering 404.
func isNotFound(err error) bool {
	var apiErr *apiclient.Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// classifyAPI turns an API refusal of a space or blob read into the
// forbidden exit; any other error keeps its own (exit 1).
func classifyAPI(err error) error {
	var apiErr *apiclient.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return withExit(ExitVaultForbidden, err)
		}
	}
	return err
}

// openRefSpace opens spaceID for the selected custody and classifies each way
// it can fail into the exit codes above.
func openRefSpace(ctx context.Context, c *apiclient.Client, vc *vaultCustody, spaceID string) (*client.Manager, error) {
	cust, err := vc.selectCustody()
	if err != nil {
		return nil, withExit(ExitVaultCustody, err)
	}
	mgr, found, err := spaceopen.Open(ctx, c, cust, spaceID)
	if err != nil {
		var apiErr *apiclient.Error
		var urlErr *url.Error
		switch {
		case errors.As(err, &apiErr):
			return nil, classifyAPI(err)
		case errors.As(err, &urlErr):
			return nil, err
		}
		return nil, withExit(ExitVaultUnwrap, fmt.Errorf("opening space %s: %w", spaceID, err))
	}
	if !found {
		return nil, withExit(ExitVaultUnwrap, fmt.Errorf("space %s holds no copy of its key wrapped for %s", spaceID, cust.RecipientID()))
	}
	return mgr, nil
}

func newVaultGetRefCommand(_ Options, configPath *string, vc *vaultCustody) *cobra.Command {
	var (
		flags   clientFlags
		outPath string
	)
	cmd := &cobra.Command{
		Use:   "get-ref hv1:<space>/<object>",
		Short: "Read one sealed object by its vault ref, decrypting it on this machine",
		Long: `Read the sealed object a vault ref names — hv1:<space uuid>/<object uuid>, the
form an outside system that never decrypts holds — and write its JSON to -o, or
to stdout. The object is the drive file ` + vaultref.ObjectDir + `/<object uuid>.json.

This is the read an executor makes (ADR-0104): with its restricted token, it
sees only the spaces an owner's device granted it, and with its sealed
recipient key (--sealed-key) it decrypts only those whose key was wrapped for
it. The plaintext goes to stdout or to the -o file (created owner-only, never
over an existing file) and nowhere else: never to stderr, never to a log. A
caller should give a path on a tmpfs it wipes.

Exit status: 3 the sealed key or its PIN is unavailable or wrong; 4 the space is
not visible to this credential (no grant, revoked, or no such space); 5 the
space cannot be decrypted with this key; 6 the ref names no object; 7 the node
served bytes that do not match their content address (a substituted or
corrupted manifest or content blob); 1 anything else.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := vaultref.Parse(args[0])
			if err != nil {
				return err
			}
			if !ref.IsObject() {
				return fmt.Errorf("%s names a collection, not an object: list it with `heyarr vault ls %s --json`", ref, ref.Space)
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				mgr, err := openRefSpace(ctx, c, vc, ref.Space)
				if err != nil {
					return err
				}
				data, err := vaultGet(ctx, c, mgr, ref.Space, ref.Path())
				if err != nil {
					return classifyOpenedRead(ref.String(), err)
				}
				defer clear(data)
				if !json.Valid(data) {
					// The content is not echoed: it may be plaintext.
					return fmt.Errorf("%s is not a JSON object", ref)
				}
				return writePlaintext(cmd.OutOrStdout(), outPath, data)
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVarP(&outPath, "out", "o", "-", "write the object to this new file (mode 0600) instead of stdout")
	return cmd
}

// writePlaintext writes decrypted bytes to stdout ("" or "-") or to a new
// owner-only file, refusing to replace one: a stale file at the path could
// otherwise be read as this object.
func writePlaintext(stdout io.Writer, outPath string, data []byte) error {
	if outPath == "" || outPath == "-" {
		_, err := stdout.Write(data)
		return err
	}
	f, err := os.OpenFile(filepath.Clean(outPath), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(outPath)
		return err
	}
	return f.Close()
}

// vaultPutRefView is the JSON put-ref prints.
type vaultPutRefView struct {
	Ref          string `json:"ref"`
	SpaceID      string `json:"space_id"`
	ObjectID     string `json:"object_id"`
	Path         string `json:"path"`
	ManifestBlob string `json:"manifest_blob"`
	Size         int64  `json:"size"`
	ChangeID     string `json:"change_id"`
}

func newVaultPutRefCommand(_ Options, configPath *string, vc *vaultCustody) *cobra.Command {
	var (
		flags clientFlags
		space string
	)
	cmd := &cobra.Command{
		Use:   "put-ref --space <space | hv1:<space>> <file | ->",
		Short: "Seal one JSON object into a vault space and print its new ref",
		Long: `Seal a JSON object, read from the file or from stdin (-), into the space under
a fresh random object id, and print its vault ref and where it landed as JSON.
The object is written at ` + vaultref.ObjectDir + `/<object uuid>.json in the space's drive.

The object must be a versioned, typed envelope: a JSON object with "v": 1 and a
non-empty "type"; the rest is the caller's. It is at most 8 MiB. The plaintext
is sealed on this machine and never uploaded, and never echoed in an error.

An executor (ADR-0104) needs a read,write grant on the space and a copy of its
key wrapped for its sealed recipient key. Exit status as for get-ref.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spaceID, err := vaultref.ParseSpace(space)
			if err != nil {
				return err
			}
			data, err := readObject(cmd.InOrStdin(), args[0])
			if err != nil {
				return err
			}
			defer clear(data)
			ref, err := vaultref.New(spaceID)
			if err != nil {
				return err
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				mgr, err := openRefSpace(ctx, c, vc, spaceID)
				if err != nil {
					return err
				}
				view, err := vaultPut(ctx, c, mgr, spaceID, ref.Path(), bytes.NewReader(data), time.Now().Unix())
				if err != nil {
					return classifyAPI(err)
				}
				return emitJSON(cmd.OutOrStdout(), vaultPutRefView{
					Ref: ref.String(), SpaceID: spaceID, ObjectID: ref.Object, Path: view.Path,
					ManifestBlob: view.ManifestBlob, Size: view.Size, ChangeID: view.ChangeID,
				})
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&space, "space", "", "the vault space: its id, or its collection ref hv1:<space uuid> (required)")
	_ = cmd.MarkFlagRequired("space")
	return cmd
}

// readObject reads a sealed object's plaintext from a file or stdin ("-") and
// checks its envelope. No error carries any of the bytes.
func readObject(stdin io.Reader, src string) ([]byte, error) {
	r := stdin
	if src != "-" {
		f, err := os.Open(filepath.Clean(src))
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxVaultObject+1))
	if err != nil {
		return nil, fmt.Errorf("reading the object: %w", err)
	}
	if len(data) > maxVaultObject {
		clear(data)
		return nil, fmt.Errorf("the object is larger than %d bytes; push a document by path with `heyarr vault push`", maxVaultObject)
	}
	var env vaultObjectEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		clear(data)
		// Not wrapped: a JSON syntax error quotes the offending byte.
		return nil, errors.New(`the object is not a JSON object`)
	}
	if env.V == nil || *env.V != 1 || env.Type == "" {
		clear(data)
		return nil, errors.New(`the object must be an envelope with "v": 1 and a non-empty "type"`)
	}
	return data, nil
}
