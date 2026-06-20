package uninstall

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/cobra"
)

var assumeYes bool

var RootCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Uninstall all components installed by nodeward",
	Long: `Uninstall Kubernetes, Containerd, Wireguard, and reset network configurations
that were installed by nodeward. This is DESTRUCTIVE: it wipes Kubernetes
(including etcd data) and reboots the node. Pass --yes to skip the prompt.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Guard: a bare `runos uninstall` must not silently destroy the node.
		// Require either --yes or an explicit "yes" confirmation. (The nodeward
		// UNINSTALL_NODE instruction calls commons.Uninstall directly and is
		// unaffected by this CLI guard.)
		if !assumeYes {
			fmt.Println("WARNING: this DESTROYS this node — it wipes Kubernetes (including etcd data),")
			fmt.Println("Containerd, WireGuard and network configuration, then reboots. Irreversible.")
			fmt.Print("Type 'yes' to proceed (or re-run with --yes): ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(line) != "yes" {
				fmt.Println("Aborted.")
				return
			}
		}
		commons.Uninstall(true)
		roslog.I("Your server will now be rebooted, ctrl+c to cancel.")
		// Wait for 5 seconds before rebooting
		time.Sleep(5 * time.Second)
		if err := commons.RebootServer(); err != nil {
			roslog.E("Failed to reboot server", err)
		}
	},
}

func init() {
	RootCmd.Flags().BoolVar(&assumeYes, "yes", false, "Skip the confirmation prompt (required for non-interactive use)")
}
