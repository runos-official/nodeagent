// Package update implements the `runos update` command, which updates the
// installed node agent binary to the advertised (or an explicitly pinned)
// version.
package update

import (
	"github.com/runos-official/nodeagent/uc/install"
	"github.com/spf13/cobra"
)

// targetVersion is bound to --version: an explicit exact-tag pin (e.g.
// "v0.24.0"). Empty means "update to the version advertised by the control plane
// (conductor)", resolved to an exact tag, never a floating "latest".
var targetVersion string

// jsonOutput is bound to --json: emit a single machine-readable result object
// instead of the human-readable banner/summary.
var jsonOutput bool

var RootCmd = &cobra.Command{
	Use:   "update",
	Short: "Update the agent to the advertised (or a pinned) version",
	Long: `Update the installed RunOS node agent binary.

With no flags, resolves the version advertised by the control plane (conductor)
for this account and updates to that exact tag. Pass an exact release tag with
--version to pin to a specific build instead. Either way the target is an exact
tag, verified against the release checksums; it never falls back to a floating
"latest". Use --json to emit a single machine-readable result object instead of
the human-readable summary.`,
	Example: `  runos update
  runos update --version v0.24.0
  runos update --json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return install.UpdateNodeAgent(targetVersion, jsonOutput)
	},
}

func init() {
	RootCmd.Flags().StringVar(&targetVersion, "version", "",
		"exact version tag to pin to (e.g. v0.24.0); defaults to the advertised version")
	RootCmd.Flags().BoolVar(&jsonOutput, "json", false,
		"emit a single machine-readable JSON result instead of human-readable output")
}
