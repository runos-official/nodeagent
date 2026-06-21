package etcd

import (
	"github.com/spf13/cobra"
)

func init() {
	RootCmd.AddCommand(listMembers)
	RootCmd.AddCommand(removeMember)
}

// RootCmd is the parent `runos etcd` command. It has no Run of its own: with no
// subcommand cobra prints help and exits non-zero, instead of silently doing
// nothing.
var RootCmd = &cobra.Command{
	Use:   "etcd",
	Short: "Manage etcd cluster members",
	Long: `Inspect and manage the local etcd cluster.

Run a subcommand:
  runos etcd list      Show cluster and per-member status
  runos etcd remove    Remove a member by --ip or --id`,
}
