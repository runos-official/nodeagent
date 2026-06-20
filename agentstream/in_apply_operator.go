package agentstream

import (
	"encoding/base64"
	"encoding/json"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/k8s"
)

// ApplyOperatorRequestType is the instruction type that applies a Kubernetes operator.
const ApplyOperatorRequestType = "APPLY_OPERATOR"

type applyOperatorRequest struct {
	CrdB64 string `json:"crdB64"`
}

// HandleApplyOperator decodes an APPLY_OPERATOR instruction and applies the
// embedded operator manifest to the cluster.
func HandleApplyOperator(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	jsonData, err := base64.StdEncoding.DecodeString(instruction.JsonB64)
	if err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	var request applyOperatorRequest
	if err := json.Unmarshal(jsonData, &request); err != nil {
		roslog.E("Error unmarshalling JSON payload", err)
		return nil, err
	}

	if err := applyOperator(request.CrdB64); err != nil {
		roslog.E("Error applying operator", err)
		return nil, err
	}

	return NoContentResponse, nil
}

func applyOperator(crdBase64Encoded string) error {
	crdBytes, err := base64.StdEncoding.DecodeString(crdBase64Encoded)
	if err != nil {
		roslog.E("Error decoding base64 encoded CRD file", err)
		return err
	}

	return k8s.ApplyYamlResourceFile(crdBytes)
}
