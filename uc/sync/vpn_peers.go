package sync

import (
	"fmt"
	"net"
	"strings"

	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// setPeers converges wg0 to EXACTLY the peer set the control plane returned, the same way the
// agent-stream path does (goal 27, vpn-peer-protocol item 3 and design-peering-mesh).
//
// This path used to be additive: it set every peer it was given and removed nothing, so a node
// that fell back to `runos sync vpn` kept retired peers and stale endpoints the stream path had
// already cleared. Both paths now plan the same convergence and add the same kernel routes for
// peers outside this node's own range, so which path delivered the peer set no longer matters.
//
// Peer fields (pubKey, IPs) are untrusted, so SetWgPeer validates them and passes each as a
// separate exec arg; this prevents shell injection. An invalid or failing peer is logged and
// skipped so one bad peer does not abort the rest. It returns how many peers were skipped so the
// caller can surface that count.
func setPeers(response *pb.ManualSyncResponse) int {
	desired := make([]commons.WgPeer, 0, len(response.GetPeers()))
	for _, peer := range response.GetPeers() {
		desired = append(desired, commons.WgPeer{
			PubKey:     peer.PubKey,
			AllowedIP:  peer.Ip,
			EndpointIP: peer.EndpointIp,
		})
	}

	// A read failure yields an empty map, which plans NO removals: the safe direction.
	current, err := commons.CurrentWgPeers()
	if err != nil {
		roslog.E("Could not read the current WireGuard peers, applying without removals", err)
	}
	plan := commons.PlanPeerConvergence(current, desired)

	for _, pubKey := range plan.Remove {
		if err := commons.RemoveWgPeer(pubKey); err != nil {
			roslog.E("Could not remove VPN peer", err, "pubKey", pubKey)
		}
	}

	skipped := 0
	for _, peer := range plan.Set {
		if err := commons.SetWgPeer(peer.PubKey, peer.AllowedIP, peer.EndpointIP); err != nil {
			roslog.E("Skipping VPN peer", err, "pubKey", peer.PubKey)
			skipped++
		}
	}

	commons.ApplyPeerRoutes(desired)
	// The ufw rules converge on BOTH sync paths, like the routes: `runos sync vpn` is the manual
	// repair tool, and a repair that fixes the routes but not the firewall repairs half the fault.
	commons.ApplyPeerUfwRules(desired)
	return skipped
}

func getWgPubKey() string {
	return strings.TrimSpace(commons.ExecuteCommandGetResponse("cat /etc/wireguard/public_key"))
}

// getWg0IPAddress returns the wg0 interface's IPv4 address. It returns an error
// (rather than aborting the process) when the interface is missing or has no
// usable IPv4, so the sync command can surface a clear remedy.
func getWg0IPAddress() (string, error) {
	iface, err := net.InterfaceByName("wg0")
	if err != nil {
		return "", fmt.Errorf("wg0 interface not found (is WireGuard installed and up? try wg show): %w", err)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("wg0 interface not found (is WireGuard installed and up? try wg show): %w", err)
	}

	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ipNet.IP.To4() != nil {
				return ipNet.IP.String(), nil
			}
		}
	}

	return "", fmt.Errorf("wg0 has no usable IPv4 address (is WireGuard installed and up? try wg show)")
}
