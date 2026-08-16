package agentstream

import (
	"time"

	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// UninstallNodeRequestType is the instruction type that uninstalls the node.
const UninstallNodeRequestType = "UNINSTALL_NODE"

// uninstallRebootDelay is how long the handler waits before rebooting, so the
// response reaches nodeward first.
const uninstallRebootDelay = 5 * time.Second

// HandleUninstallNode uninstalls Kubernetes, Containerd and WireGuard from the node,
// then reboots it.
//
// Goal 23 review, F9-c. The handler used to swallow commons.Uninstall's error and
// never reboot, so `nodes delete` left a partially wiped node with kube-apiserver
// and cilium-agent still running (containerd's reparented shim children survive
// a `systemctl stop containerd`). Nodeward treats a non-answer as offline, so
// nothing upstream noticed. The reboot runs after a short delay in a goroutine
// so the response goes out first, and it runs on the failure path too: only a
// reboot clears an orphaned API server.
func HandleUninstallNode() (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleUninstallNode")
	if err := commons.Uninstall(true); err != nil {
		roslog.E("Uninstall left components behind; rebooting anyway so no orphaned Kubernetes process survives", err)
	}
	go func() {
		time.Sleep(uninstallRebootDelay)
		if err := commons.RebootServer(); err != nil {
			roslog.E("Uninstall finished but the node did not reboot", err)
		}
	}()

	return NoContentResponse, nil
}
