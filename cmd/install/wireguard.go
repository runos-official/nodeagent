package install

import (
	"github.com/runos-official/nodeagent/roslog"
	registernode "github.com/runos-official/nodeagent/uc/install"
	"github.com/runos-official/nodeagent/uc/sync"
	"github.com/spf13/cobra"
)

var wireguardCmd = &cobra.Command{
	Use:   "wireguard",
	Short: "Install Wireguard only",
	Run: func(cmd *cobra.Command, args []string) {
		// Install Wireguard
		roslog.Println("Installing Wireguard")
		if err := registernode.InstallVpn(); err != nil {
			roslog.Printf("Fatal error: %v\n", err)
			roslog.E("VPN installation failed", err)
			return
		}

		// Sync peer so that we can join a cluster
		roslog.Println("Syncing VPN")
		if err := sync.ForceVpnSync(); err != nil {
			roslog.Printf("Fatal error: %v\n", err)
			roslog.E("VPN sync failed", err)
			return
		}
	},
}
