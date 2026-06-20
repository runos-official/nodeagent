package sync

import (
	"log"
	"net"
	"strings"

	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

func setPeers(response *pb.ManualSyncResponse) {
	// Configure each peer via a direct (non-shell) wg invocation. Peer fields
	// (pubKey, IPs) are untrusted, so SetWgPeer validates them and passes each
	// as a separate exec arg; this prevents shell injection. An invalid or
	// failing peer is logged and skipped so one bad peer does not abort the rest.
	// sudo wg set wg0 peer <Node2PublicKey> allowed-ips 10.0.0.2/32 endpoint <Node2PublicIP>:51820 persistent-keepalive 5
	for _, peer := range response.GetPeers() {
		if err := commons.SetWgPeer(peer.PubKey, peer.Ip, peer.EndpointIp); err != nil {
			roslog.E("Skipping VPN peer", err, "pubKey", peer.PubKey)
		}
	}
}

func getWgPubKey() string {
	return strings.TrimSpace(commons.ExecuteCommandGetResponse("cat /etc/wireguard/public_key"))
}

func getWg0IPAddress() string {
	iface, err := net.InterfaceByName("wg0")
	if err != nil {
		log.Fatalf("Failed to get wg0 interface")
	}

	addrs, err := iface.Addrs()
	if err != nil {
		log.Fatalf("Failed to get addresses for wg0")
	}

	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ipNet.IP.To4() != nil {
				return ipNet.IP.String()
			}
		}
	}

	return ""
}
