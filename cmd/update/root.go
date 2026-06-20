// Package update implements the `runos update` command, which updates the
// installed node agent binary to the advertised (or an explicitly pinned)
// version.
package update

import (
	"github.com/runos-official/nodeagent/uc/install"
	"github.com/spf13/cobra"
)

// targetVersion is bound to --version: an explicit exact-tag pin (e.g.
// "v0.24.0"). Empty means "update to the advertised version" (current behavior).
var targetVersion string

var RootCmd = &cobra.Command{
	Use:   "update",
	Short: "Update the agent to the advertised (or a pinned) version",
	Run: func(cmd *cobra.Command, args []string) {
		install.UpdateNodeAgent(targetVersion)
	},
}

func init() {
	RootCmd.Flags().StringVar(&targetVersion, "version", "",
		"exact version tag to pin to (e.g. v0.24.0); defaults to the advertised version")
}
