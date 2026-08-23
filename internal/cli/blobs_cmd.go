package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

func newBlobsCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "blobs",
		Short: "Inspect and read stored bytes",
		Long: `Bytes are identified by their BLAKE3 digest and by nothing else (ADR-0005).

A blob identifier is blake3:<64 lowercase hex characters>. A malformed one is
rejected as a mistake in what you typed; a well-formed one this peer does not
hold is reported as absent. They are different answers to different questions
and the CLI keeps them apart.`,
	}
	cmd.AddCommand(
		newBlobsStatCommand(opts, configPath),
		newBlobsCatCommand(opts, configPath),
		newBlobsVerifyCommand(opts, configPath),
	)
	return cmd
}

func newBlobsStatCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "stat <hash>",
		Short: "Show what the catalog knows about a blob",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				blob, err := c.StatBlob(ctx, args[0])
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), blob)
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "  hash        %s\n", blob.Hash)
				fmt.Fprintf(out, "  size        %d\n", blob.Size)
				fmt.Fprintf(out, "  mime        %s\n", dash(blob.MIME))
				// The three-way answer, not the boolean. `chunked false`
				// meant both "these bytes never need a manifest" and "nobody
				// has looked", and an operator deciding whether to chunk
				// something needs to know which (§16, ADR-0034).
				fmt.Fprintf(out, "  manifest    %s\n", blob.ChunkManifest)
				fmt.Fprintf(out, "  first seen  %s\n", stamp(blob.FirstSeenAt))
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

// catOutput is the --json shape of `heyarr blobs cat`.
type catOutput struct {
	Hash         string `json:"hash"`
	Output       string `json:"output"`
	BytesWritten int64  `json:"bytes_written"`
	// TotalBytes is the blob's full length, or -1 when the server did not say.
	TotalBytes int64 `json:"total_bytes"`
	// Resumed reports whether an existing partial file was continued rather
	// than restarted.
	Resumed bool `json:"resumed"`
	// StartedFrom is the offset the transfer began at.
	StartedFrom int64 `json:"started_from"`
}

func newBlobsCatCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags  clientFlags
		output string
		resume bool
	)
	cmd := &cobra.Command{
		Use:   "cat <hash>",
		Short: "Write a blob's bytes to a file or to stdout",
		Long: `Read a blob's bytes (ADR-0013).

With --resume and an output file that already exists, the transfer continues
from the end of that file: the request carries Range: bytes=<offset>- and
If-Range with the blob's own validator, which is derived from the digest, so
nothing has to be remembered between runs. If the server answers 200 rather
than 206 the range was not honoured, and the only correct response is to start
over — appending a whole object to a partial one produces a file that is the
right length for nothing.

--json requires --output. Writing a JSON summary and the bytes to the same
stream would produce neither.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.asJSON && output == "" {
				return errors.New("blobs cat: --json needs --output, or the summary and the bytes " +
					"would be interleaved on stdout")
			}
			if resume && output == "" {
				return errors.New("blobs cat: --resume needs --output — there is nothing to resume on stdout")
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				summary, err := catBlob(ctx, c, cmd.OutOrStdout(), args[0], output, resume)
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), summary)
				}
				if output != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes to %s%s\n",
						summary.BytesWritten, summary.Output, resumeNote(summary))
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "", "write to this file instead of stdout")
	cmd.Flags().BoolVar(&resume, "resume", false, "continue an interrupted transfer into --output")
	return cmd
}

func resumeNote(s catOutput) string {
	if s.Resumed {
		return fmt.Sprintf(" (resumed from %d)", s.StartedFrom)
	}
	return ""
}

// catBlob streams a blob to stdout or to a file.
func catBlob(ctx context.Context, c *client.Client, stdout io.Writer,
	hash, output string, resume bool,
) (catOutput, error) {
	summary := catOutput{Hash: hash, Output: "-", TotalBytes: -1}

	var offset int64
	if resume && output != "" {
		if info, err := os.Stat(output); err == nil {
			offset = info.Size()
		} else if !errors.Is(err, os.ErrNotExist) {
			return summary, fmt.Errorf("checking %s: %w", output, err)
		}
	}

	content, err := c.OpenBlobContent(ctx, hash, offset)
	if err != nil {
		return summary, err
	}
	defer func() { _ = content.Body.Close() }()

	summary.TotalBytes = content.Total
	summary.Resumed = content.Resumed
	summary.StartedFrom = content.Offset

	if output == "" {
		n, err := io.Copy(stdout, content.Body)
		summary.BytesWritten = n
		return summary, err
	}

	summary.Output = output
	// Append only when the server confirmed the range. Otherwise truncate: the
	// bytes on disk are not a prefix of what is arriving.
	mode := os.O_CREATE | os.O_WRONLY
	if content.Resumed && offset > 0 {
		mode |= os.O_APPEND
	} else {
		mode |= os.O_TRUNC
	}
	file, err := os.OpenFile(filepath.Clean(output), mode, 0o600)
	if err != nil {
		return summary, fmt.Errorf("opening %s: %w", output, err)
	}
	n, copyErr := io.Copy(file, content.Body)
	summary.BytesWritten = n
	if closeErr := file.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr
	}
	return summary, copyErr
}

func newBlobsVerifyCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "verify <hash>",
		Short: "Read a blob back and check that it hashes to its own name",
		Long: `Download a blob and hash the bytes as they arrive.

The check is done here, on the bytes as received, rather than by asking the
server whether they are fine — invariant 1: a destination always verifies bytes
itself and never trusts a claimed hash. Asking the server would confirm only
that its catalog agrees with itself.

It exits non-zero when the bytes do not match, so it can be a cron job.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				result, err := c.VerifyBlob(ctx, args[0])
				if err != nil {
					return err
				}
				if flags.asJSON {
					if err := emitJSON(cmd.OutOrStdout(), result); err != nil {
						return err
					}
				} else {
					out := cmd.OutOrStdout()
					fmt.Fprintf(out, "  hash         %s\n", result.Hash)
					fmt.Fprintf(out, "  size         %d\n", result.Size)
					fmt.Fprintf(out, "  bytes read   %d\n", result.BytesRead)
					fmt.Fprintf(out, "  hashes to    %s\n", result.ActualHash)
					fmt.Fprintf(out, "  verified     %t\n", result.Verified)
					if result.Detail != "" {
						fmt.Fprintf(out, "  problem      %s\n", result.Detail)
					}
				}
				if !result.Verified {
					return fmt.Errorf("blobs verify: %s did not verify: %s", result.Hash, result.Detail)
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}
