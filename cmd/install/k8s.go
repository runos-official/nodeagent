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
	Run: func(cmd *cobra.Command, args []string) {
		// Sync peer so that we can join a cluster
		roslog.Println("Syncing VPN")
		if err := sync.ForceVpnSync(); err != nil {
			roslog.Printf("Fatal error: %v\n", err)
			roslog.E("VPN sync failed", err)
			return
		}

		// Install K8s, assuming we now have vpn connectivity to the target peer node to join the cluster.
		roslog.Println("Installing K8s")
		if err := install.K8s(); err != nil {
			roslog.E("K8s installation failed", err)
			return
		}
	},
}
