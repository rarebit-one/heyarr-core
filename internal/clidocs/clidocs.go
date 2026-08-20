// Package clidocs generates the CLI reference under docs/cli.
//
// The reference is generated from the cobra command tree and committed, and CI
// asserts that regenerating it produces no diff (the "generated code is
// current" job runs scripts/gen.sh and then `git diff --exit-code`). Written
// documentation for a command surface drifts the first time a flag is renamed;
// generated documentation cannot, because the generator is the code.
//
// # Why this renders the pages itself rather than calling cobra/doc
//
// github.com/spf13/cobra/doc is the obvious tool and it was the first choice.
// It is one package with GenMarkdownTree, GenManTree and GenYamlTree in it, so
// importing it pulls go-md2man and blackfriday into the module graph — a
// markdown-to-roff renderer, for man pages this project does not ship, in a
// dependency set that is otherwise small and deliberate. Everything below is
// read off the cobra command tree: the names, the flag sets and their usage
// strings are cobra's, and only the markdown is ours. That is a hundred lines
// against two dependencies in every downstream build's go.sum.
//
// Two things make the output deterministic, which is what lets it be a diff
// check rather than a nightly regeneration:
//
//   - Nothing derived from the clock, the environment or the filesystem is
//     written. cobra/doc's "Auto generated on <date>" footer is exactly the
//     kind of thing that fails the diff check daily and teaches everyone to
//     ignore it.
//   - Stale files are removed. A command that is deleted would otherwise leave
//     its page behind forever: `git diff` sees nothing, because the file is
//     still committed and still unchanged.
package clidocs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/rarebit-one/heyarr-core/internal/cli"
)

// IndexFile is the front door to the generated pages.
const IndexFile = "README.md"

// Generate writes the reference into dir, replacing whatever was there.
func Generate(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("clidocs: creating %s: %w", dir, err)
	}

	root := cli.NewRootCommand(cli.Options{})
	root.DisableAutoGenTag = true

	written := map[string]bool{IndexFile: true}
	if err := writeTree(root, dir, written); err != nil {
		return err
	}
	if err := writeIndex(root, filepath.Join(dir, IndexFile)); err != nil {
		return err
	}
	return removeStale(dir, written)
}

// writeTree renders one page per command.
func writeTree(cmd *cobra.Command, dir string, written map[string]bool) error {
	if !documented(cmd) {
		return nil
	}
	name := pageName(cmd)
	written[name] = true
	if err := os.WriteFile(filepath.Join(dir, name), []byte(render(cmd)), 0o600); err != nil {
		return fmt.Errorf("clidocs: writing %s: %w", name, err)
	}
	for _, child := range cmd.Commands() {
		if err := writeTree(child, dir, written); err != nil {
			return err
		}
	}
	return nil
}

// documented reports whether a command gets a page. Hidden and generated help
// commands do not: documenting `heyarr help` tells nobody anything.
func documented(cmd *cobra.Command) bool {
	return cmd.Root() == cmd || (!cmd.Hidden && cmd.Name() != "help")
}

// pageName is the file a command's page lives in: the command path with spaces
// replaced, which is the convention cobra/doc established and which existing
// links elsewhere expect.
func pageName(cmd *cobra.Command) string {
	return strings.ReplaceAll(cmd.CommandPath(), " ", "_") + ".md"
}

// render produces one command's page.
func render(cmd *cobra.Command) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## %s\n\n", cmd.CommandPath())
	fmt.Fprintf(&b, "%s\n\n", cmd.Short)

	if long := strings.TrimSpace(cmd.Long); long != "" {
		b.WriteString("### Synopsis\n\n")
		b.WriteString(long)
		b.WriteString("\n\n")
	}

	if cmd.Runnable() {
		b.WriteString("```\n")
		b.WriteString(cmd.UseLine())
		b.WriteString("\n```\n\n")
	}

	if example := strings.TrimSpace(cmd.Example); example != "" {
		b.WriteString("### Examples\n\n```\n")
		b.WriteString(example)
		b.WriteString("\n```\n\n")
	}

	writeFlags(&b, "Options", cmd.NonInheritedFlags())
	writeFlags(&b, "Options inherited from parent commands", cmd.InheritedFlags())

	var links []string
	if parent := cmd.Parent(); parent != nil {
		links = append(links, fmt.Sprintf("* [%s](%s)\t - %s",
			parent.CommandPath(), pageName(parent), parent.Short))
	}
	var children []string
	for _, child := range cmd.Commands() {
		if !documented(child) {
			continue
		}
		children = append(children, fmt.Sprintf("* [%s](%s)\t - %s",
			child.CommandPath(), pageName(child), child.Short))
	}
	sort.Strings(children)
	links = append(links, children...)
	if len(links) > 0 {
		b.WriteString("### See also\n\n")
		b.WriteString(strings.Join(links, "\n"))
		b.WriteString("\n")
	}

	return b.String()
}

// writeFlags renders a flag set, or nothing when it is empty.
func writeFlags(b *strings.Builder, heading string, flags *pflag.FlagSet) {
	if !flags.HasAvailableFlags() {
		return
	}
	fmt.Fprintf(b, "### %s\n\n```\n%s```\n\n", heading, flags.FlagUsages())
}

// writeIndex renders a table of the commands, so the directory has a front
// door rather than thirty files named after themselves.
func writeIndex(root *cobra.Command, path string) error {
	var b strings.Builder
	b.WriteString("# heyarr command reference\n\n")
	b.WriteString("Generated from the cobra command tree by `make gen`. Do not edit these pages by\n")
	b.WriteString("hand: CI regenerates them and fails on any difference.\n\n")
	b.WriteString("The root command is documented in [`heyarr.md`](heyarr.md).\n\n")
	b.WriteString("| Command | Description |\n| --- | --- |\n")

	var rows []string
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if !documented(cmd) {
			return
		}
		if cmd.Parent() != nil {
			rows = append(rows, fmt.Sprintf("| [`%s`](%s) | %s |\n",
				cmd.CommandPath(), pageName(cmd), cmd.Short))
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
	sort.Strings(rows)
	for _, row := range rows {
		b.WriteString(row)
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("clidocs: writing %s: %w", path, err)
	}
	return nil
}

// removeStale deletes markdown files no command produced.
func removeStale(dir string, keep map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("clidocs: reading %s: %w", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") || keep[name] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("clidocs: removing the stale page %s: %w", name, err)
		}
	}
	return nil
}
