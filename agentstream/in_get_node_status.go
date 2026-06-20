package agentstream

import (
	"encoding/base64"
	"encoding/json"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/k8s"
)

const (
	// GetNodeStatusRequestType is the instruction type requesting node readiness.
	GetNodeStatusRequestType = "GET_NODE_STATUS"
	// GetNodeStatusResponseType is the response type carrying node status.
	GetNodeStatusResponseType = "NODE_STATUS"
)

type getNodeStatusRequest struct {
}

type getNodeStatusResponse struct {
	IsReady bool `json:"nodeIsReady"`
}

// HandleGetNodeStatus reports the node's current Kubernetes readiness back to Nodeward.
func HandleGetNodeStatus(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	//log.Println("Executing HandleGetNodeStatus")
	jsonData, err := base64.StdEncoding.DecodeString(instruction.JsonB64)
	if err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	var request getNodeStatusRequest
	if err := json.Unmarshal(jsonData, &request); err != nil {
		roslog.E("Error unmarshalling JSON payload", err)
		return nil, err
	}

	response := getNodeStatusResponse{
		IsReady: k8s.IsNodeReady(),
	}

	responseJson, err := json.Marshal(response)
	if err != nil {
		roslog.E("Error marshalling response JSON", err)
		return nil, err
	}
	responseJsonB64 := base64.StdEncoding.EncodeToString(responseJson)

	return &pb.FromNodeAgent{
		JsonB64: responseJsonB64,
		Type:    GetNodeStatusResponseType,
	}, nil
}
