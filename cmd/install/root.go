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

		// Install K8s, assuming we now have vpn connectivity to the target peer node to join the cluster.
		roslog.Println("Installing K8s")
		if err := registernode.K8s(); err != nil {
			roslog.E("K8s installation failed", err)
			return
		}

		if err := backend.AddNodelog(3, "NodeInstallation", "Installation completed successfully"); err != nil {
			roslog.E("Error adding nodelog", err)
		}
	},
}
