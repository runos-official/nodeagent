package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"time"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/config"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

const (
	// SetVpnPeersRequestType is the instruction type configuring WireGuard peers.
	SetVpnPeersRequestType = "SET_VPN_PEERS"
	// SetVpnPeersResponseType is the response type acknowledging a peer update.
	SetVpnPeersResponseType = "NODE_VPN_PEER"
)

type vpnPeerRequest struct {
	Peers []vpnPeer `json:"peers"`
}

type vpnPeerResponse struct {
	Peer vpnPeer `json:"nodePeer"`
}

type vpnPeer struct {
	PubKey     string `json:"pubKey"`
	EndpointIP string `json:"endpointIp"`
	VpnIP      string `json:"vpnIp"`
	// Pool addresses of the VMs the peer node hosts (goal 27, vm-group-range-routing).
	ExtraAllowedIPs []string `json:"extraAllowedIps,omitempty"`
}

// HandleSetVpnPeers decodes a SET_VPN_PEERS instruction and configures the
// node's WireGuard peers accordingly.
func HandleSetVpnPeers(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	//log.Println("Executing HandleSetVpnPeers")
	jsonData, err := base64.StdEncoding.DecodeString(instruction.JsonB64)
	if err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	var request vpnPeerRequest
	if err := json.Unmarshal(jsonData, &request); err != nil {
		roslog.E("Error unmarshalling JSON payload", err)
		return nil, err
	}

	setVpnPeers(request)

	vpnIp, err := getWg0IPAddress()
	if err != nil {
		roslog.E("Error getting WG0 IP address", err)
		return nil, err
	}

	vpnPeerResponse := vpnPeerResponse{
		Peer: vpnPeer{
			PubKey:     getWgPubKey(),
			EndpointIP: config.GetNodeIP(),
			VpnIP:      vpnIp,
		},
	}

	// Encode the response as JSON and then to Base64
	responseJson, err := json.Marshal(vpnPeerResponse)
	if err != nil {
		roslog.E("Error marshalling response JSON", err)
		return nil, err
	}
	responseJsonB64 := base64.StdEncoding.EncodeToString(responseJson)

	// Prepare the response
	return &pb.FromNodeAgent{
		JsonB64: responseJsonB64,
		Type:    SetVpnPeersResponseType,
	}, nil
}

// setVpnPeers converges wg0 to EXACTLY the peer set it was sent (goal 27, vpn-peer-protocol).
//
// It used to be additive: it looped over the supplied peers and set each one, and removed nothing.
// So a retired node stayed a peer with a working key, and a peer once given the wrong endpoint
// kept it forever, because there was no way to say "no endpoint". Both were permanent and silent.
//
// Peer fields are untrusted, so SetWgPeer validates them and passes each as a separate exec
// argument; that has not changed. A single failing peer is logged and skipped so one bad peer does
// not abort the rest.
func setVpnPeers(request vpnPeerRequest) {
	desired := make([]commons.WgPeer, 0, len(request.Peers))
	for _, peer := range request.Peers {
		desired = append(desired, commons.WgPeer{
			PubKey:          peer.PubKey,
			AllowedIP:       peer.VpnIP,
			EndpointIP:      peer.EndpointIP,
			ExtraAllowedIPs: peer.ExtraAllowedIPs,
		})
	}

	// A read failure yields an empty map, which plans NO removals. That is the safe direction:
	// treating "could not read" as "no peers are configured" would tear down every working tunnel
	// on this node.
	current, err := commons.CurrentWgPeerStates()
	if err != nil {
		roslog.E("Could not read the current WireGuard peers, applying without removals", err)
	}

	plan := commons.PlanPeerConvergence(current, desired, time.Now())

	for _, pubKey := range plan.Remove {
		if err := commons.RemoveWgPeer(pubKey); err != nil {
			roslog.E("Could not remove VPN peer", err, "pubKey", pubKey)
		}
	}

	for _, peer := range plan.Set {
		if err := commons.SetWgPeer(peer.PubKey, peer.AllowedIP, peer.EndpointIP, peer.ExtraAllowedIPs); err != nil {
			roslog.E("Skipping VPN peer", err, "pubKey", peer.PubKey)
		}
	}

	// KERNEL ROUTES FOR PEERS OUTSIDE THIS NODE'S OWN RANGE (goal 27, design-peering-mesh). A peer
	// from a peered cluster is accepted by WireGuard's allowed-ips but the kernel has no route to
	// it, so its traffic would leave by the default gateway. Converged to exactly the desired set,
	// like the peers themselves; a no-op on a cluster with no peerings.
	commons.ApplyPeerRoutes(desired)

	// ufw ALLOW RULES FOR THE PEERED-CLUSTER RANGES (goal 27, peering-loose-ends). The install-time
	// rules allow this node's own range and legacy 172.24/16 only, so a node with ufw active drops
	// a peered cluster's traffic even with the route in place. Converged with the peer set; a no-op
	// when ufw is absent or inactive.
	commons.ApplyPeerUfwRules(desired)
}

func getWgPubKey() string {
	res, err := commons.ExecuteDirectCommandGetResponse("cat", false, "/etc/wireguard/public_key")
	if err != nil {
		roslog.E("Error getting WireGuard public key", err)
		return ""
	}
	return strings.TrimSpace(*res)
}

func getWg0IPAddress() (string, error) {
	iface, err := net.InterfaceByName("wg0")
	if err != nil {
		roslog.E("Failed to get interface wg0", err)
		return "", err
	}

	addresses, err := iface.Addrs()
	if err != nil {
		roslog.E("Failed to get addresses for wg0", err)
		return "", err
	}

	for _, address := range addresses {
		if ipNet, ok := address.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ipNet.IP.To4() != nil {
				return ipNet.IP.String(), nil
			}
		}
	}

	return "", nil
}
