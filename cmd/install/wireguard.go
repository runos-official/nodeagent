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
	RunE: func(cmd *cobra.Command, args []string) error {
		// Install Wireguard
		roslog.Println("Installing Wireguard")
		if err := registernode.InstallVpn(); err != nil {
			return roslog.Fail("Install WireGuard", err.Error(),
				"check connectivity to Nodeward on TCP 9192 and run 'sudo runos preflight', then re-run")
		}

		// Sync peer so that we can join a cluster
		roslog.Println("Syncing VPN")
		if err := sync.ForceVpnSync(); err != nil {
			return roslog.Fail("Sync VPN peers", err.Error(),
				"verify WireGuard came up ('wg show') and Nodeward is reachable, then re-run")
		}
		return nil
	},
}
