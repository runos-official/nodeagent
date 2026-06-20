package sync

import (
	"fmt"
	"github.com/spf13/cobra"
)

func init() {
	RootCmd.AddCommand(vpnCmd)
}

var RootCmd = &cobra.Command{
	Use:   "sync",
	Short: "Manually run a sync",

	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Not supported")
	},
}
