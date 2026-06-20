package certificate

import (
	"github.com/spf13/cobra"
)

// RootCmd represents the certificate command
var RootCmd = &cobra.Command{
	Use:   "certificate",
	Short: "Manage node agent certificates",
	Long:  `Manage mTLS certificates used by the node agent for secure communication with Nodeward`,
}

func init() {
	// Add subcommands
	RootCmd.AddCommand(renewCmd)
}
