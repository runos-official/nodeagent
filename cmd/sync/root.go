package sync

import (
	"github.com/spf13/cobra"
)

func init() {
	RootCmd.AddCommand(vpnCmd)
}

var RootCmd = &cobra.Command{
	Use:   "sync",
	Short: "Manually resynchronize node state",
	Long: `Manually resynchronize this node's state with the Nodeward control plane.

Run a subcommand to choose what to sync (for example, WireGuard VPN peers).`,
	Example: "  sudo runos sync vpn",
	// No positional args: bare `runos sync` lists its subcommands via Help below.
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// No subcommand given: show help so the operator sees the available
		// sync targets instead of a misleading "Not supported" success.
		return cmd.Help()
	},
}
