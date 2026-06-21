package sync

import (
	"fmt"

	"github.com/runos-official/nodeagent/roslog"
	syncUc "github.com/runos-official/nodeagent/uc/sync"
	"github.com/spf13/cobra"
)

var vpnCmd = &cobra.Command{
	Use:   "vpn",
	Short: "Resynchronize WireGuard VPN peers with Nodeward",
	Long: `Run a manual WireGuard VPN peer sync against the Nodeward control plane.

The agent advertises this node's wg0 IPv4 address and public key to Nodeward and
applies the peer set returned in response. Use this when peer connectivity looks
stale (e.g. after a node IP change) without waiting for the next automatic sync.

Requires WireGuard to be installed and up (wg show) and Nodeward reachable on
TCP 9192. Run with sudo so the wg interface can be reconfigured.`,
	Example: "  sudo runos sync vpn",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		roslog.Println("Syncing VPN...")

		skipped, err := syncUc.ForceVpnSyncWithCount()
		if err != nil {
			return roslog.Fail(
				"Sync VPN peers",
				err.Error(),
				"verify WireGuard is up (wg show) and Nodeward reachable on TCP 9192, then re-run sudo runos sync vpn",
			)
		}

		if skipped > 0 {
			roslog.Println(fmt.Sprintf("VPN sync complete (%d peer(s) skipped, see /var/log/runos.log).", skipped))
		} else {
			roslog.Println("VPN sync complete.")
		}
		return nil
	},
}
