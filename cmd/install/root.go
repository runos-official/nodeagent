package install

import (
	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/roslog"
	registernode "github.com/runos-official/nodeagent/uc/install"
	"github.com/runos-official/nodeagent/uc/sync"
	"github.com/spf13/cobra"
)

func init() {
	RootCmd.AddCommand(k8sCmd)
	RootCmd.AddCommand(wireguardCmd)
}

var RootCmd = &cobra.Command{
	Use:   "install",
	Short: "Install this node",
	Long:  `Install this node`,
	// RunE (not Run) so a failed install propagates a non-zero exit code to the
	// installer script instead of silently exiting 0.
	RunE: func(cmd *cobra.Command, args []string) error {
		// Install Wireguard
		roslog.Println("Installing Wireguard")
		if err := registernode.InstallVpn(); err != nil {
			return roslog.Fail("Install WireGuard", err.Error(),
				"check connectivity to Nodeward on TCP 9192 and run 'sudo runos preflight', then re-run 'sudo runos install'")
		}

		// Sync peer so that we can join a cluster
		roslog.Println("Syncing VPN")
		if err := sync.ForceVpnSync(); err != nil {
			return roslog.Fail("Sync VPN peers", err.Error(),
				"verify WireGuard came up ('wg show') and Nodeward is reachable, then re-run 'sudo runos install'")
		}

		// Install K8s, assuming we now have vpn connectivity to the target peer node to join the cluster.
		roslog.Println("Installing K8s")
		if err := registernode.K8s(); err != nil {
			return roslog.Fail("Install Kubernetes", err.Error(),
				"see /var/log/runos.log and 'journalctl -u runos'; run 'sudo runos preflight' to diagnose, then re-run 'sudo runos install'")
		}

		if err := backend.AddNodelog(3, "NodeInstallation", "Installation completed successfully"); err != nil {
			roslog.E("Error adding nodelog", err)
		}
		return nil
	},
}
