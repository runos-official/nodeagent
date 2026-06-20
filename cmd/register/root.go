package register

import (
	"github.com/runos-official/nodeagent/uc/registernode"
	"github.com/spf13/cobra"
	"log"
	"os/exec"
	"strings"
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
	Run: func(cmd *cobra.Command, args []string) {
		registernode.RegisterNode(token, aid, getMachineId(), cp, server)
	},
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

// getMachineId calls local system command to get the Linux machine id. The machine id is a unique identifier
// that is generated when the system is installed. It is used to identify the system in a network.
func getMachineId() string {
	cmd := "cat /etc/machine-id"
	out, err := exec.Command("/bin/sh", "-c", cmd).Output()
	if err != nil {
		log.Fatalf("Error getting machine id: %v", err)
	}
	return strings.TrimSpace(string(out))
}
