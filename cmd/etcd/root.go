package etcd

import (
	"github.com/spf13/cobra"
)

func init() {
	RootCmd.AddCommand(listMembers)
	RootCmd.AddCommand(removeMember)
}

var RootCmd = &cobra.Command{
	Use:   "etcd",
	Short: "Manage Etcd",
	Run: func(cmd *cobra.Command, args []string) {
	},
}
