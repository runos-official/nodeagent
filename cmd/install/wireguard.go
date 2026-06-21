package install

import (
	"github.com/runos-official/nodeagent/roslog"
	registernode "github.com/runos-official/nodeagent/uc/install"
	"github.com/runos-official/nodeagent/uc/sync"
	"github.com/spf13/cobra"
)

var wireguardCmd = &cobra.Command{
	Use:   "wireguard",
	Short: "Install WireGuard only (then sync VPN peers)",
	Long: `Install WireGuard on this node and sync its VPN peers from Nodeward.

Brings up the overlay interface and pulls the peer configuration so the node
can reach the cluster, without installing Kubernetes. Use this to re-run only
the networking phase before 'runos install k8s'. Requires root and a prior
successful 'runos register'.`,
	Example: `  # Install WireGuard and sync peers only:
  sudo runos install wireguard`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Install Wireguard
		banner("Installing WireGuard")
		if err := registernode.InstallVpn(); err != nil {
			return roslog.Fail("Install WireGuard", err.Error(),
				"check connectivity to Nodeward on TCP 9192 and run 'sudo runos preflight', then re-run")
		}

		// Sync peer so that we can join a cluster
		banner("Syncing VPN")
		if err := sync.ForceVpnSync(); err != nil {
			return roslog.Fail("Sync VPN peers", err.Error(),
				"verify WireGuard came up ('wg show') and Nodeward is reachable, then re-run")
		}
		return nil
	},
}
