package kubeproxy

import (
	"fmt"
	"github.com/runos-official/nodeagent/agentstream"
	"log"

	"github.com/spf13/cobra"
)

var (
	force bool
)

var refreshBackends = &cobra.Command{
	Use:   "refresh",
	Short: "Refresh HAProxy backends by updating kube-proxy",
	Run: func(cmd *cobra.Command, args []string) {
		if err := agentstream.ForceKubeproxyUpdate(); err != nil {
			log.Fatalf("Error refreshing HAProxy backends: %v", err)
		}

		fmt.Println("Successfully refreshed HAProxy backends")
	},
}

func init() {
	refreshBackends.Flags().BoolVar(&force, "force", false, "Force refresh even if no changes detected")
}
