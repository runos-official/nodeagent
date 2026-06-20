package install

import (
	"context"
	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"log"
	"time"
)

// InstallVpn installs WireGuard on this node by fetching and running the VPN
// install command list from Nodeward.
func InstallVpn() error {
	c, _, backendCancel, conn := backend.NodewardL2Sec()
	defer backendCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer conn.Close()
	request := &pb.GetInstallVpnCommandsRequest{}
	res, err := c.GetInstallVpnCommands(ctx, request)
	if err != nil {
		log.Fatalf("Error executing GetInstallVpnCommands: %v", err)
	}

	return commons.ProcessInstallCommandsStatusAware(res)
}
