package sync

import (
	"fmt"
	"net"
	"strings"

	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// setPeers configures each peer via a direct (non-shell) wg invocation. Peer
// fields (pubKey, IPs) are untrusted, so SetWgPeer validates them and passes
// each as a separate exec arg; this prevents shell injection. An invalid or
// failing peer is logged and skipped so one bad peer does not abort the rest.
// It returns how many peers were skipped so the caller can surface that count.
// sudo wg set wg0 peer <Node2PublicKey> allowed-ips 10.0.0.2/32 endpoint <Node2PublicIP>:51820 persistent-keepalive 5
func setPeers(response *pb.ManualSyncResponse) int {
	skipped := 0
	for _, peer := range response.GetPeers() {
		if err := commons.SetWgPeer(peer.PubKey, peer.Ip, peer.EndpointIp); err != nil {
			roslog.E("Skipping VPN peer", err, "pubKey", peer.PubKey)
			skipped++
		}
	}
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
