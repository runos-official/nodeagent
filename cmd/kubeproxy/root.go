package kubeproxy

import (
	"github.com/spf13/cobra"
)

func init() {
	RootCmd.AddCommand(listBackends)
	RootCmd.AddCommand(refreshBackends)
}

var RootCmd = &cobra.Command{
	Use:   "kubeproxy",
	Short: "Manage HAProxy",
	Run: func(cmd *cobra.Command, args []string) {
	},
}
