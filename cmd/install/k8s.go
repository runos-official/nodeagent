package install

import (
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/install"
	"github.com/runos-official/nodeagent/uc/sync"
	"github.com/spf13/cobra"
)

var k8sCmd = &cobra.Command{
	Use:   "k8s",
	Short: "Install Kubernetes, assumes VPN is already installed",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Sync peer so that we can join a cluster
		roslog.Println("Syncing VPN")
		if err := sync.ForceVpnSync(); err != nil {
			return roslog.Fail("Sync VPN peers", err.Error(),
				"verify WireGuard came up ('wg show') and Nodeward is reachable, then re-run")
		}

		// Install K8s, assuming we now have vpn connectivity to the target peer node to join the cluster.
		roslog.Println("Installing K8s")
		if err := install.K8s(); err != nil {
			return roslog.Fail("Install Kubernetes", err.Error(),
				"see /var/log/runos.log and 'journalctl -u runos'; run 'sudo runos preflight' to diagnose, then re-run")
		}
		return nil
	},
}
