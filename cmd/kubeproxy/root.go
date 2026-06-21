package kubeproxy

import (
	"github.com/spf13/cobra"
)

func init() {
	RootCmd.AddCommand(listBackends)
	RootCmd.AddCommand(refreshBackends)
}

// RootCmd is the parent for the `runos kubeproxy` subcommands. It carries no
// RunE of its own: invoked bare it prints help (cobra's default when a parent
// command has subcommands but no Run/RunE).
var RootCmd = &cobra.Command{
	Use:   "kubeproxy",
	Short: "Manage the local HAProxy Kubernetes API load balancer",
	Long: `Manage the local HAProxy load balancer that fronts the Kubernetes API
across the cluster's control-plane nodes (listening on port 6446).

Use the subcommands to inspect the active backends or force a refresh of the
backend server list from Nodeward.`,
	Example: `  runos kubeproxy list
  runos kubeproxy refresh`,
}
