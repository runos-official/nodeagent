package install

import (
	"context"
	"fmt"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
)

// InstallVpn installs WireGuard on this node by fetching and running the VPN
// install command list from Nodeward.
func InstallVpn() error {
	c, _, backendCancel, conn, err := backend.NodewardL2Sec()
	if err != nil {
		return err
	}
	defer backendCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer conn.Close()
	request := &pb.GetInstallVpnCommandsRequest{}
	res, err := c.GetInstallVpnCommands(ctx, request)
	if err != nil {
		// Return (never Fatalf) so the install exits non-zero with an actionable
		// message. 9192 is a separate port from registration's 9191; many
		// firewalls open one but not the other.
		return fmt.Errorf("could not fetch VPN install commands from Nodeward: %w (check egress to the Nodeward operations channel on TCP 9192)", err)
	}

	return commons.ProcessInstallCommandsStatusAware(res)
}
