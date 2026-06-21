package install

import (
	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/roslog"
	registernode "github.com/runos-official/nodeagent/uc/install"
	"github.com/runos-official/nodeagent/uc/sync"
	"github.com/spf13/cobra"
)

// quiet suppresses the step banners ("Installing WireGuard", "Syncing VPN",
// "Installing Kubernetes") for non-interactive installer-script / cloud-init
// use. It is a persistent flag, so the install subcommands inherit it. The live
// progress bar already auto-suppresses on a non-TTY (see roslog); this flag also
// silences the banners when a TTY happens to be attached (e.g. cloud-init with a
// pseudo-terminal) so captured output stays terse.
var quiet bool

func init() {
	RootCmd.PersistentFlags().BoolVar(&quiet, "quiet", false,
		"suppress step banners for installer-script/cloud-init use")
	// --no-progress is an alias for --quiet (clearer intent in cloud-init).
	RootCmd.PersistentFlags().BoolVar(&quiet, "no-progress", false,
		"alias for --quiet")

	RootCmd.AddCommand(k8sCmd)
	RootCmd.AddCommand(wireguardCmd)
}

// banner prints a phase banner via roslog.InstallInfo so it shares the
// timestamp/icon style of the rest of the install output and does not visually
// collide with the carriage-return progress redraws. Suppressed under --quiet.
func banner(msg string) {
	if quiet {
		return
	}
	roslog.InstallInfo(msg)
}

var RootCmd = &cobra.Command{
	Use:   "install",
	Short: "Install this node (WireGuard, VPN sync, then Kubernetes)",
	Long: `Install this node into its RunOS cluster.

Runs the full node install in three phases, in order:

  1. Install WireGuard and bring up the overlay interface.
  2. Sync VPN peers from Nodeward so this node can reach the cluster.
  3. Install Kubernetes and join the cluster over the VPN.

Requires root (it writes system config and installs packages) and a prior
successful 'runos register' (the mTLS certificates under /etc/runos must
already exist). Run 'sudo runos preflight' first if you want to validate the
host before installing.

The 'wireguard' and 'k8s' subcommands run individual phases for recovery or
debugging; the top-level 'install' runs all three.

For installer scripts and cloud-init, pass --quiet (alias --no-progress) to
suppress the step banners. The live progress bar already collapses to plain
log lines automatically when stdout is not a terminal.`,
	Example: `  # Full install (run after a successful register):
  sudo runos install

  # Non-interactive (installer script / cloud-init):
  sudo runos install --quiet

  # Re-run only the WireGuard + VPN-sync phase:
  sudo runos install wireguard

  # Re-run only the Kubernetes phase (VPN already up):
  sudo runos install k8s`,
	Args: cobra.NoArgs,
	// RunE (not Run) so a failed install propagates a non-zero exit code to the
	// installer script instead of silently exiting 0.
	RunE: func(cmd *cobra.Command, args []string) error {
		// Install Wireguard
		banner("Installing WireGuard")
		if err := registernode.InstallVpn(); err != nil {
			return roslog.Fail("Install WireGuard", err.Error(),
				"check connectivity to Nodeward on TCP 9192 and run 'sudo runos preflight', then re-run 'sudo runos install'")
		}

		// Sync peer so that we can join a cluster
		banner("Syncing VPN")
		if err := sync.ForceVpnSync(); err != nil {
			return roslog.Fail("Sync VPN peers", err.Error(),
				"verify WireGuard came up ('wg show') and Nodeward is reachable, then re-run 'sudo runos install'")
		}

		// Install K8s, assuming we now have vpn connectivity to the target peer node to join the cluster.
		banner("Installing Kubernetes")
		if err := registernode.K8s(); err != nil {
			return roslog.Fail("Install Kubernetes", err.Error(),
				"see /var/log/runos.log and 'journalctl -u runos'; run 'sudo runos preflight' to diagnose, then re-run 'sudo runos install'")
		}

		// Best-effort completion record. A failure here does not fail the install
		// (Kubernetes is already up), but the operator should know the completion
		// was not recorded on Nodeward, so emit a visible warning and keep exit 0.
		if err := backend.AddNodelog(3, "NodeInstallation", "Installation completed successfully"); err != nil {
			roslog.E("Error adding nodelog", err)
			roslog.InstallWarning("Installation completed, but could not record completion to Nodeward; the node will still appear once it reports in")
		}
		return nil
	},
}
