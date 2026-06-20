package register

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/registernode"
	"github.com/spf13/cobra"
)

var (
	aid    string
	server string
	token  string
	cp     string
)

var RootCmd = &cobra.Command{
	Use:   "register",
	Short: "Register this node",
	Long:  `Register this node`,
	// RunE so a failed registration exits non-zero (the installer checks it).
	RunE: func(cmd *cobra.Command, args []string) error {
		// Validate required flags BEFORE any work, so an empty --server can't
		// silently persist an empty nodeward host (which later dials ":9191" and
		// fails with an opaque TLS error).
		if err := validateRegisterFlags(); err != nil {
			return err
		}

		machineId, err := getMachineId()
		if err != nil {
			return roslog.Fail("Register node", err.Error(),
				"generate a machine-id with 'sudo systemd-machine-id-setup' (or 'sudo dbus-uuidgen --ensure'), then re-run")
		}

		if err := registernode.RegisterNode(token, aid, machineId, cp, server); err != nil {
			return roslog.Fail("Register node", err.Error(),
				"re-copy the registration command from the RunOS console (tokens are short-lived) and re-run")
		}
		return nil
	},
}

// validateRegisterFlags rejects missing required flags with a clear message,
// before any network or config work.
func validateRegisterFlags() error {
	if strings.TrimSpace(server) == "" {
		return fmt.Errorf("missing required --server (the Nodeward host). Use the full registration command from the RunOS console; do not run register by hand")
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("missing required --token (the short-lived registration token from the console)")
	}
	if strings.TrimSpace(aid) == "" {
		return fmt.Errorf("missing required --aid (the account ID from the console)")
	}
	return nil
}

func init() {
	RootCmd.PersistentFlags().StringVarP(&aid, "aid", "a", "",
		"Account ID")
	RootCmd.PersistentFlags().StringVarP(&server, "server", "s", "",
		"Installer Server")
	RootCmd.PersistentFlags().StringVarP(&token, "token", "t", "",
		"Secret, short-lived token, provided to you when requesting the registration command.")
	RootCmd.PersistentFlags().StringVarP(&cp, "control-plane", "c", "0",
		"Whether this node will be a control plane node 1 or 0. Default is 0 if you already "+
			"have at least 3 cp nodes in your cluster.")
}

// getMachineId returns the Linux machine id, a unique identifier generated at
// system install time and used to identify this node. It falls back through the
// known locations before failing, since minimal/cloned images sometimes lack
// /etc/machine-id.
func getMachineId() (string, error) {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if data, err := os.ReadFile(path); err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				return id, nil
			}
		}
	}
	// Last resort: a generator, if present.
	if out, err := exec.Command("/bin/sh", "-c", "cat /etc/machine-id").Output(); err == nil {
		if id := strings.TrimSpace(string(out)); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("cannot read /etc/machine-id (required to identify this node); the file is missing or empty")
}
