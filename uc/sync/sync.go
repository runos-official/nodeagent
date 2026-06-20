package sync

import (
	"context"
	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/config"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/k8s"
	"time"
)

// ForceVpnSync performs a manual VPN peer sync with Nodeward.
func ForceVpnSync() error {
	nodeIsReady := k8s.IsNodeReady()
	c, _, backendCancel, conn, err := backend.NodewardL2Sec()
	if err != nil {
		return err
	}
	defer backendCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer conn.Close()

	// First request does a basic sync only. The response from this might include additional
	// instructions to perform.
	request := &pb.ManualSyncRequest{
		NodePeer: &pb.VpnPeer{
			PubKey:     getWgPubKey(),
			EndpointIp: config.GetNodeIP(),
			Ip:         getWg0IPAddress(),
		},
		NodeIsReady: nodeIsReady,
	}

	roslog.I("Running manual VPN sync", "request", request)

	response, err := c.ManualSync(ctx, request)
	if err != nil {
		return err
	}

	roslog.I("Setting VPN Peers")
	setPeers(response)

	return nil
}
