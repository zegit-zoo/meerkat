// Package main renders the cobra command tree as a single-file
// markdown reference. Run via `go run ./internal/clidocs > docs/CLI.md`
// (the Makefile wraps this in `make docs`).
//
// Single-file output (vs cobra/doc's per-command tree) keeps
// docs/CLI.md greppable, browseable, and rendering cleanly on the
// GitLab repo landing page.
package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/cli"
)

func main() {
	root := cli.NewRootCmd()
	w := os.Stdout

	fmt.Fprintf(w, "# meerkat CLI reference\n\n")
	fmt.Fprintf(w, "Auto-generated from the cobra command tree.\n")
	fmt.Fprintf(w, "Do NOT edit by hand — run `make docs` to regenerate.\n\n")
	fmt.Fprintf(w, "Source of truth: `internal/cli/*.go`. "+
		"Source generator: `internal/clidocs/clidocs.go`.\n\n")

	fmt.Fprintf(w, "## Synopsis\n\n```\n%s\n```\n\n", strings.TrimSpace(root.Long))
	if root.Example != "" {
		fmt.Fprintf(w, "## Examples\n\n```sh\n%s\n```\n\n", strings.TrimSpace(root.Example))
	}

	fmt.Fprintf(w, "## Commands\n\n")
	for _, g := range root.Groups() {
		fmt.Fprintf(w, "### %s\n\n", strings.TrimSuffix(g.Title, ":"))
		for _, c := range commandsInGroup(root, g.ID) {
			fmt.Fprintf(w, "- [`%s`](#%s) — %s\n",
				c.CommandPath(), anchor(c.CommandPath()), c.Short)
		}
		fmt.Fprintf(w, "\n")
	}

	fmt.Fprintf(w, "## Reference\n\n")
	walk(w, root)
}

func commandsInGroup(root *cobra.Command, gid string) []*cobra.Command {
	var out []*cobra.Command
	for _, c := range root.Commands() {
		if c.GroupID == gid {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// walk renders cmd plus every sub-command depth-first.
func walk(w io.Writer, cmd *cobra.Command) {
	render(w, cmd)
	subs := cmd.Commands()
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name() < subs[j].Name() })
	for _, sub := range subs {
		// Skip cobra's auto-generated 'completion' and 'help' — they
		// add noise without value.
		if sub.Name() == "completion" || sub.Name() == "help" {
			continue
		}
		walk(w, sub)
	}
}

func render(w io.Writer, cmd *cobra.Command) {
	if cmd.Hidden {
		return
	}
	path := cmd.CommandPath()
	if path == "" {
		path = cmd.Use
	}
	fmt.Fprintf(w, "### `%s`\n\n", path)
	if cmd.Short != "" {
		fmt.Fprintf(w, "%s\n\n", cmd.Short)
	}
	if cmd.Long != "" && cmd.Long != cmd.Short {
		fmt.Fprintf(w, "%s\n\n", strings.TrimSpace(cmd.Long))
	}
	fmt.Fprintf(w, "**Usage**\n\n```\n%s\n```\n\n", cmd.UseLine())

	if subs := visibleSubcommands(cmd); len(subs) > 0 {
		fmt.Fprintf(w, "**Subcommands**\n\n")
		for _, s := range subs {
			fmt.Fprintf(w, "- `%s` — %s\n", s.Name(), s.Short)
		}
		fmt.Fprintf(w, "\n")
	}

	if cmd.HasAvailableLocalFlags() {
		fmt.Fprintf(w, "**Flags**\n\n```\n%s```\n\n", cmd.LocalFlags().FlagUsages())
	}
	if cmd.HasAvailableInheritedFlags() {
		fmt.Fprintf(w, "**Inherited flags**\n\n```\n%s```\n\n", cmd.InheritedFlags().FlagUsages())
	}

	if cmd.Example != "" {
		fmt.Fprintf(w, "**Examples**\n\n```sh\n%s\n```\n\n", strings.TrimSpace(cmd.Example))
	}
	fmt.Fprintf(w, "---\n\n")
}

func visibleSubcommands(cmd *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, s := range cmd.Commands() {
		if s.Hidden || s.Name() == "completion" || s.Name() == "help" {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// anchor turns "meerkat ingest sources" into "meerkat-ingest-sources"
// for markdown header anchors (GitLab/GitHub flavoured).
func anchor(s string) string {
	s = strings.ReplaceAll(s, " ", "-")
	return strings.ToLower(s)
}
