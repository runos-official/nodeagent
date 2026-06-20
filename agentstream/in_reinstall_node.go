package agentstream

import (
	"fmt"
	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"os"
	"os/exec"
)

// ReinstallNodeRequestType is the instruction type that reinstalls the node.
const ReinstallNodeRequestType = "REINSTALL_NODE"

// HandleReinstallNode queues the reinstall command as a oneshot service,
// uninstalls the node, and reboots so it comes back up freshly reinstalled.
func HandleReinstallNode(in *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleReinstallNode")

	type requestType struct {
		ReinstallCommand string `json:"reinstallCommand"`
	}
	var request requestType
	if err := commons.JSONB64Decode(in.JsonB64, &request); err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	if err := QueueReinstallCommand(request.ReinstallCommand); err != nil {
		roslog.E("Error queuing reinstall command", err)
		return nil, err
	}

	_ = backend.UpdateStatus("UNINSTALLING")

	commons.Uninstall(false)

	if err := commons.RebootServer(); err != nil {
		roslog.E("Error rebooting server", err)
		return nil, err
	}

	return NoContentResponse, nil
}

// QueueReinstallCommand writes and enables a oneshot systemd service that runs
// installCmd on next boot, so reinstallation survives the reboot.
func QueueReinstallCommand(installCmd string) error {
	// Create a oneshot systemd service
	serviceContent := fmt.Sprintf(`[Unit]
Description=RunOS Node Reinstallation
After=network.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=oneshot
ExecStart=/bin/bash -c '%s && systemctl disable runos-reinstall.service'
RemainAfterExit=false

[Install]
WantedBy=multi-user.target
`, installCmd)

	// Write service file (root-only: may embed install command text)
	if err := os.WriteFile("/etc/systemd/system/runos-reinstall.service", []byte(serviceContent), 0600); err != nil {
		return err
	}

	// Reload systemd daemon
	reloadCmd := exec.Command("systemctl", "daemon-reload")
	if err := reloadCmd.Run(); err != nil {
		return err
	}

	// Enable the service
	enableCmd := exec.Command("systemctl", "enable", "runos-reinstall.service")
	if err := enableCmd.Run(); err != nil {
		return err
	}

	return nil
}
