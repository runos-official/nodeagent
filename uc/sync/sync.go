package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/config"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/k8s"
)

// ForceVpnSync performs a manual VPN peer sync with Nodeward, discarding the
// skipped-peer count. Callers that want to surface how many peers were skipped
// should use ForceVpnSyncWithCount instead.
func ForceVpnSync() error {
	_, err := ForceVpnSyncWithCount()
	return err
}

// ForceVpnSyncWithCount performs a manual VPN peer sync with Nodeward. It
// returns the number of peers that were skipped (invalid / failed to apply) so
// the caller can surface that to the operator, plus any fatal error.
func ForceVpnSyncWithCount() (int, error) {
	// Resolve and validate the local WireGuard identity before opening the RPC,
	// so we never advertise blank values to Nodeward.
	wg0IP, err := getWg0IPAddress()
	if err != nil {
		return 0, err
	}

	pubKey := getWgPubKey()
	if pubKey == "" {
		return 0, fmt.Errorf("WireGuard public key is empty (is WireGuard installed? expected /etc/wireguard/public_key)")
	}

	nodeIsReady := k8s.IsNodeReady()
	c, _, backendCancel, conn, err := backend.NodewardL2Sec()
	if err != nil {
		return 0, err
	}
	defer backendCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer conn.Close()

	// First request does a basic sync only. The response from this might include additional
	// instructions to perform.
	request := &pb.ManualSyncRequest{
		NodePeer: &pb.VpnPeer{
			PubKey:     pubKey,
			EndpointIp: config.GetNodeIP(),
			Ip:         wg0IP,
		},
		NodeIsReady: nodeIsReady,
	}

	roslog.I("Running manual VPN sync", "request", request)

	response, err := c.ManualSync(ctx, request)
	if err != nil {
		return 0, err
	}

	roslog.I("Setting VPN Peers")
	skipped := setPeers(response)

	return skipped, nil
}
