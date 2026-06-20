package agentstream

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
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

// reinstallScriptPath is the fixed, root-only location of the reinstall script.
// The systemd unit references this path literally; the caller-supplied command
// is written into the file (argv-safe), never interpolated into the unit body.
const reinstallScriptPath = "/var/lib/runos/reinstall.sh"

// QueueReinstallCommand writes and enables a oneshot systemd service that runs
// installCmd on next boot, so reinstallation survives the reboot.
//
// Security: installCmd is written verbatim into a root-only 0600 script file at a
// FIXED path and the unit's ExecStart points at that fixed path. Nothing from
// installCmd is interpolated into the unit body, which removes both the systemd
// unit-directive injection and the shell injection that the old
// `ExecStart=/bin/bash -c '%s ...'` interpolation allowed.
func QueueReinstallCommand(installCmd string) error {
	if strings.TrimSpace(installCmd) == "" {
		return fmt.Errorf("reinstall command is empty")
	}

	// Write the reinstall command to a root-only script file (argv-safe write:
	// the command text is the file content, not part of any shell/unit string).
	if err := os.MkdirAll(filepath.Dir(reinstallScriptPath), 0700); err != nil {
		return fmt.Errorf("failed to create reinstall script dir: %w", err)
	}
	scriptBody := "#!/bin/bash\n" + installCmd + "\n"
	if err := os.WriteFile(reinstallScriptPath, []byte(scriptBody), 0600); err != nil {
		return fmt.Errorf("failed to write reinstall script: %w", err)
	}

	// Create a oneshot systemd service whose ExecStart is a FIXED path. The unit
	// body contains no caller-controlled text.
	serviceContent := fmt.Sprintf(`[Unit]
Description=RunOS Node Reinstallation
After=network.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=oneshot
ExecStart=/bin/bash %s
ExecStartPost=/usr/bin/systemctl disable runos-reinstall.service
RemainAfterExit=false

[Install]
WantedBy=multi-user.target
`, reinstallScriptPath)

	// Write service file (root-only: references the reinstall script path)
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
