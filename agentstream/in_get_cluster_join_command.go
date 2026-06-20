package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"os/exec"
	"strings"

	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

const (
	// GetClusterJoinCommandRequestType is the instruction type requesting a kubeadm join command.
	GetClusterJoinCommandRequestType = "GET_CLUSTER_JOIN_CMD"
	// GetClusterJoinCommandResponseType is the response type carrying the join command.
	GetClusterJoinCommandResponseType = "CLUSTER_JOIN_CMD"
)

type getClusterJoinCommandRequest struct {
	AsCp bool `json:"asCp"`
}

type getClusterJoinCommandResponse struct {
	JoinCmd       string `json:"joinCmd"`
	EncryptionKey string `json:"encryptionKey,omitempty"`
}

// HandleGetClusterJoinCommand returns a kubeadm join command for the requesting
// node, including the control-plane certificate key when joining as a control plane.
func HandleGetClusterJoinCommand(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleGetClusterJoinCommand")
	jsonData, err := base64.StdEncoding.DecodeString(instruction.JsonB64)
	if err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	var request getClusterJoinCommandRequest
	if err := json.Unmarshal(jsonData, &request); err != nil {
		roslog.E("Error unmarshalling JSON payload", err)
		return nil, err
	}

	response := getClusterJoinCommandResponse{
		//JoinCmd: joinCmd,
	}

	//var joinCmd string
	if request.AsCp {
		response.JoinCmd = getClusterJoinCommandAsCp()
		response.EncryptionKey = getEncryptionKey()
		//joinCmd = getClusterJoinCommandAsCp()
	} else {
		response.JoinCmd = getMinJoinCommand()
		//joinCmd = getMinJoinCommand()
	}
	//response := getClusterJoinCommandResponse{
	//	JoinCmd: joinCmd,
	//}

	responseJson, err := json.Marshal(response)
	if err != nil {
		roslog.E("Error marshalling response JSON", err)
		return nil, err
	}
	responseJsonB64 := base64.StdEncoding.EncodeToString(responseJson)

	return &pb.FromNodeAgent{
		JsonB64: responseJsonB64,
		Type:    GetClusterJoinCommandResponseType,
	}, nil
}

// GetClusterJoinCommandAsCp
// kubeadm join <control-plane-endpoint>:6443 --token <token> --discovery-token-ca-cert-hash sha256:<hash> --control-plane --certificate-key 074ab7df6359cb2c21e6a6e10c255065b162c7332ba231eec33a7e18fbd77a10
func getClusterJoinCommandAsCp() string {
	return getMinJoinCommand() + " --control-plane --certificate-key " + getCertKey()
}

// GetClusterJoinCommandAsWorker
// kubeadm join cp:6443 --token vpns94.v31cyd1flfmgi7le --discovery-token-ca-cert-hash sha256:41e2312c18834ff4393a0f31dda148c84aa36ba508479bcae262bc621e14629f
func getClusterJoinCommandAsWorker() string {
	return getMinJoinCommand()
}

func getMinJoinCommand() string {
	cmd := "kubeadm token create --print-join-command"
	out, err := exec.Command("/bin/sh", "-c", cmd).Output()
	if err != nil {
		roslog.E("Error getting machine id", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

func getEncryptionKey() string {
	data, err := commons.ExecuteDirectCommandGetResponse("cat", false, "/etc/kubernetes/enc/key.txt")
	if err != nil {
		roslog.E("Error getting admin.conf", err)
		return ""
	}
	return *data
}

func getCertKey() string {
	cmd := "kubeadm init phase upload-certs --upload-certs"
	out, err := exec.Command("/bin/sh", "-c", cmd).Output()
	if err != nil {
		roslog.E("Error getting certificate key", err)
		return ""
	}
	output := strings.TrimSpace(string(out))

	lines := strings.Split(output, "\n")

	// Ensure that we capture the correct line containing the certificate key
	var certKey string
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			certKey = strings.TrimSpace(line)
		}
	}

	return certKey
}
