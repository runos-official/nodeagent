package uninstall

import (
	"bufio"
	"errors"
	"fmt"
	"io"
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
	Example: `  # Prompt for confirmation, then uninstall and reboot
  runos uninstall

  # Non-interactive: skip the confirmation prompt (for scripts)
  runos uninstall --yes`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Guard: a bare `runos uninstall` must not silently destroy the node.
		// Require either --yes or an explicit "yes" confirmation. (The nodeward
		// UNINSTALL_NODE instruction calls commons.Uninstall directly and is
		// unaffected by this CLI guard.)
		//
		// The warning, prompt, and abort messages go to STDERR so they are not
		// captured as program output; only step results go to STDOUT.
		if !assumeYes {
			fmt.Fprintln(os.Stderr, "WARNING: this DESTROYS this node — it wipes Kubernetes (including etcd data),")
			fmt.Fprintln(os.Stderr, "Containerd, WireGuard and network configuration, then reboots. Irreversible.")
			fmt.Fprint(os.Stderr, "Type 'yes' to proceed (or re-run with --yes): ")
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			answer := strings.TrimSpace(line)
			if err != nil && !(errors.Is(err, io.EOF) && answer != "") {
				// Read error, or EOF with no buffered input (piped/closed stdin):
				// do not wipe. Report on stderr and exit non-zero, no full block.
				fmt.Fprintln(os.Stderr, "Aborted (no confirmation received).")
				return roslog.AlreadyReported(fmt.Errorf("aborted: no confirmation received"))
			}
			if !strings.EqualFold(answer, "yes") {
				fmt.Fprintln(os.Stderr, "Aborted.")
				return roslog.AlreadyReported(fmt.Errorf("aborted by operator"))
			}
		}

		uninstallErr := commons.Uninstall(true)
		if uninstallErr != nil {
			// A partial wipe must NOT look identical to a clean one: surface the
			// exact load-bearing steps that failed.
			// The remedy names the reboot FIRST (goal 23, F9). A partial uninstall stops every
			// systemd unit and deletes every config directory while leaving kube-apiserver
			// listening on 6443 and cilium-agent running, because containerd's shim children are
			// reparented rather than stopped. Re-running the uninstall cannot clear that, and
			// neither can `kubeadm reset -f`: only a reboot does. Measured on all four campaign
			// hosts, still serving on 6443 more than two hours later.
			//
			// Interactive: do not reboot, so the operator can inspect the node before it
			// disappears. --yes (goal 23 review, F9-a): nobody is there to inspect, and a node
			// left half-wiped with a live API server is the worse outcome, so reboot anyway.
			if assumeYes {
				roslog.E("Partial uninstall; rebooting anyway because --yes was given", uninstallErr)
				roslog.Println("Partial uninstall: " + uninstallErr.Error())
				roslog.Println("Rebooting anyway (--yes) so no orphaned Kubernetes process survives. Inspect /var/log/runos.log for the failing step.")
				time.Sleep(5 * time.Second)
				if err := commons.RebootServer(); err != nil {
					return roslog.Fail(
						"Reboot node",
						err.Error(),
						"partial uninstall and the node did not reboot; reboot manually with `sudo systemctl reboot`",
					)
				}
				// Rebooting does not make the wipe clean. A script that reads exit 0 as
				// "uninstalled" would move on to a re-join that the leftovers then break.
				return roslog.Fail("Uninstall node", uninstallErr.Error(), "partial uninstall; the node is rebooting to clear orphaned processes")
			}
			return roslog.Fail(
				"Uninstall node",
				uninstallErr.Error(),
				"some components were not fully removed, and this node has NOT been rebooted. "+
					"Kubernetes processes orphaned from containerd may still be running and holding "+
					"ports 6443/8472, which blocks the next install. REBOOT THIS NODE "+
					"(`sudo systemctl reboot`), then re-run `runos uninstall` if anything remains. "+
					"Inspect /var/log/runos.log for the failing step",
			)
		}

		roslog.Println("Uninstallation complete.")
		roslog.Println("Your server will now be rebooted, ctrl+c to cancel.")
		// Wait for 5 seconds before rebooting
		time.Sleep(5 * time.Second)
		if err := commons.RebootServer(); err != nil {
			return roslog.Fail(
				"Reboot node",
				err.Error(),
				"node uninstalled but did not reboot; reboot manually with `sudo systemctl reboot`",
			)
		}
		return nil
	},
}

func init() {
	RootCmd.Flags().BoolVar(&assumeYes, "yes", false, "Skip the confirmation prompt (required for non-interactive use)")
}
