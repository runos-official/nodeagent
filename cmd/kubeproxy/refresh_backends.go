package kubeproxy

import (
	"github.com/runos-official/nodeagent/agentstream"
	"github.com/runos-official/nodeagent/roslog"

	"github.com/spf13/cobra"
)

var refreshBackends = &cobra.Command{
	Use:   "refresh",
	Short: "Refresh HAProxy backends from Nodeward",
	Long: `Fetch the current control-plane node list directly from Nodeward and rewrite
the local HAProxy backend so the Kubernetes API load balancer points at the
live control-plane nodes.

If Nodeward reports no control-plane nodes the existing configuration is left
untouched and this command reports that nothing was refreshed.`,
	Example: `  runos kubeproxy refresh`,
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		roslog.Println("Refreshing HAProxy backends from Nodeward...")

		count, err := agentstream.ForceKubeproxyUpdate()
		if err != nil {
			return roslog.Fail("Refresh HAProxy backends", err.Error(),
				"check the node is registered and Nodeward is reachable; see /var/log/runos.log")
		}

		if count == 0 {
			roslog.Println("No control-plane nodes reported by Nodeward; HAProxy backends left unchanged")
			return nil
		}

		roslog.Printf("Successfully refreshed HAProxy backends (%d control-plane node(s))\n", count)
		return nil
	},
}
