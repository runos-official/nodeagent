package install

import (
	"github.com/runos-official/nodeagent/roslog"
	"os/exec"
)

func executeCommand(command string) error {
	_, err := executeCommandGetResponse(command)
	return err
}

func executeCommandGetResponse(command string) (string, error) {
	roslog.I("Executing command", "command", command)

	// Create an *exec.Cmd
	systemCmd := exec.Command("/bin/sh", "-c", command)

	// Run the command and capture its output
	output, err := systemCmd.CombinedOutput()
	if err != nil {
		roslog.E("Command execution failed", err)
		return string(output), err
	}

	roslog.I("Command output", "output", string(output))

	return string(output), nil
}
