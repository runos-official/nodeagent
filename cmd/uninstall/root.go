package uninstall

import (
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/cobra"
	"time"
)

var RootCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Uninstall all components installed by nodeward",
	Long:  `Uninstall Kubernetes, Containerd, Wireguard, and reset network configurations that were installed by nodeward.`,
	Run: func(cmd *cobra.Command, args []string) {
		commons.Uninstall(true)
		roslog.I("Your server will now be rebooted, ctrl+c to cancel.")
		// Wait for 5 seconds before rebooting
		time.Sleep(5 * time.Second)
		if err := commons.RebootServer(); err != nil {
			roslog.E("Failed to reboot server", err)
		}
	},
}
