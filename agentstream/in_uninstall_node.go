package agentstream

import (
	"fmt"
	"os/exec"

	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// UninstallNodeRequestType is the instruction type that uninstalls the node.
const UninstallNodeRequestType = "UNINSTALL_NODE"

// uninstallStartDelaySeconds is how long the detached unit waits before it starts
// the uninstall, so this response reaches nodeward first.
const uninstallStartDelaySeconds = 3

// HandleUninstallNode schedules the uninstall of Kubernetes, containerd and WireGuard,
// followed by a reboot, in a transient systemd unit that outlives this process, and
// answers at once.
//
// Goal 23 review, F9-c, second pass. The first fix ran commons.Uninstall(true) inline
// and rebooted from a goroutine. That cannot work: Uninstall's last steps are
// `systemctl stop runos`, which SIGTERMs this very process (KillMode=control-group),
// so the response was never sent and the goroutine died before it rebooted. Nodeward
// treated the silence as "offline" and the box kept an orphaned kube-apiserver when
// `kubeadm reset` had failed. Measured on the goal 23 reset of 8go, 2026-08-16: every
// machine was wiped, none rebooted by itself.
//
// So the work moves out of the agent's cgroup: `runos uninstall --yes` runs in a
// systemd-run unit (its own scope, not stopped with runos.service) and reboots on
// success and on a partial wipe alike, because only a reboot clears a shim-orphaned
// API server. The reply says "scheduled", which is the truth: the uninstall has not
// happened yet when nodeward reads it, and the node record is deleted regardless.
func HandleUninstallNode() (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleUninstallNode: scheduling a detached uninstall and reboot")
	if err := scheduleDetachedUninstall(uninstallStartDelaySeconds); err != nil {
		roslog.E("Could not schedule the detached uninstall; nothing was removed", err)
		return nil, err
	}
	return NoContentResponse, nil
}

// scheduleDetachedUninstall starts `runos uninstall --yes` and then a reboot inside a
// transient systemd unit, after `delay` seconds. systemd-run puts the unit in its own
// cgroup, so `systemctl stop runos` (which the uninstall itself runs) does not kill it.
// The reboot runs unconditionally: `runos uninstall --yes` already reboots on a partial
// wipe, and on a clean wipe the machine reboots so cilium links, DRBD modules and any
// reparented process are gone and the box comes back joinable.
func scheduleDetachedUninstall(delay int) error {
	script := fmt.Sprintf(
		"sleep %d; /usr/local/bin/runos uninstall --yes; systemctl reboot",
		delay,
	)
	path, err := exec.LookPath("systemd-run")
	if err != nil {
		// No systemd-run: fall back to a setsid'd shell, which also survives the agent's
		// stop because it is reparented to init rather than to runos.service.
		cmd := exec.Command("setsid", "/bin/sh", "-c", script)
		if startErr := cmd.Start(); startErr != nil {
			return fmt.Errorf("setsid fallback failed: %w", startErr)
		}
		roslog.I("Detached uninstall scheduled via setsid", "pid", cmd.Process.Pid)
		return nil
	}
	cmd := exec.Command(path,
		"--collect",
		"--description", "RunOS node uninstall and reboot",
		"/bin/sh", "-c", script,
	)
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		return fmt.Errorf("systemd-run failed: %v (%s)", runErr, string(out))
	}
	roslog.I("Detached uninstall scheduled via systemd-run", "delaySeconds", delay)
	return nil
}
