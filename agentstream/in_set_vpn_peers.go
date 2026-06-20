package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"

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

func setVpnPeers(request vpnPeerRequest) {
	// Configure each peer via a direct (non-shell) wg invocation. Peer fields
	// (pubKey, IPs) are untrusted, so SetWgPeer validates them and passes each
	// as a separate exec arg; this prevents shell injection. An invalid or
	// failing peer is logged and skipped so one bad peer does not abort the rest.
	for _, peer := range request.Peers {
		if err := commons.SetWgPeer(peer.PubKey, peer.VpnIP, peer.EndpointIP); err != nil {
			roslog.E("Skipping VPN peer", err, "pubKey", peer.PubKey)
		}
	}
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
