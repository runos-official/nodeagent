package agentstream

import (
	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"log"
)

// UninstallNodeRequestType is the instruction type that uninstalls the node.
const UninstallNodeRequestType = "UNINSTALL_NODE"

// HandleUninstallNode uninstalls Kubernetes, Containerd and WireGuard from the node.
func HandleUninstallNode() (*pb.FromNodeAgent, error) {
	log.Println("Executing HandleUninstallNode")
	commons.Uninstall(true)

	return NoContentResponse, nil
}
