package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
)

// collections.go holds the CLI's collection plumbing: the registry the
// root command resolves once per invocation, the shared --collection
// flag, and the one rule every subcommand needs about how to print a
// page ID.

// activeRegistry is the set of collections resolved by the root
// command's PersistentPreRunE, before any subcommand's RunE runs.
//
// It is nil when a subcommand is constructed and executed directly —
// which the package's own tests do routinely, driving newListCmd() et
// al against a kb.UseFS fixture without a root command. registry()
// covers that case, so a nil here always means "whatever the process
// globals point at", never a crash.
var activeRegistry *collections.Registry

// activeAuth is the `auth:` block of the content-source.yaml resolved
// by the root command's PersistentPreRunE, or nil when there is none
// (which is every deployment that hasn't opted in). Only `mk mcp
// serve-http` reads it; see internal/authz.
var activeAuth *authz.Config

// registry returns the collections this invocation serves, falling back
// to a single collection over the process-global KB filesystem.
func registry() *collections.Registry {
	if activeRegistry != nil {
		return activeRegistry
	}
	return collections.Global(kbSourceProvenance)
}

// addCollectionFlag registers the shared --collection flag on cmd,
// storing its value in target. verb is the subcommand's own word for
// what it does, so each help line reads naturally.
func addCollectionFlag(cmd *cobra.Command, target *string, verb string) {
	cmd.Flags().StringVar(target, "collection", "",
		"Only "+verb+" this named collection (see 'mk list --collections'). "+
			"Default: every mounted collection. Single-collection deployments can ignore this.")
	_ = cmd.RegisterFlagCompletionFunc("collection", completeCollections)
}

// completeCollections completes --collection from the mounted set.
func completeCollections(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	var out []string
	for _, n := range registry().Names() {
		if strings.HasPrefix(n, toComplete) {
			out = append(out, n)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// displayID renders a page reference's ID for human-readable output:
// the bare page ID when only one collection is mounted (identical to
// every pre-collections deployment), and the qualified
// "<collection>:<page-id>" form when several are — which is exactly
// what can be pasted back into `mk show`.
//
// Machine-readable output (--json) never rewrites an ID: it reports the
// unqualified `id` and a separate `collection` field, so anything that
// consumes an ID keeps working unchanged.
func displayID(reg *collections.Registry, ref collections.PageRef) string {
	if reg.Single() {
		return ref.Page.ID
	}
	return ref.QualifiedID()
}
