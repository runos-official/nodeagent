package sync

import (
	"github.com/runos-official/nodeagent/roslog"
	syncUc "github.com/runos-official/nodeagent/uc/sync"
	"github.com/spf13/cobra"
)

var vpnCmd = &cobra.Command{
	Use:   "vpn",
	Short: "Sync VPN",
	Run: func(cmd *cobra.Command, args []string) {
		roslog.Println("Syncing VPN...")
		syncUc.ForceVpnSync()
	},
}
