package agentstream

import (
	"encoding/base64"

	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/k8s"
)

// ApplyCRRequestType is the instruction type that applies a Kubernetes custom resource.
const ApplyCRRequestType = "APPLY_CR"

type applyCRRequest struct {
	CrB64 string `json:"crB64"`
}

// HandleApplyCR decodes an APPLY_CR instruction and applies the embedded custom
// resource manifest to the cluster.
func HandleApplyCR(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleApplyCR")

	var request applyCRRequest
	if err := commons.JSONB64Decode(instruction.JsonB64, &request); err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	if err := applyCR(request.CrB64); err != nil {
		roslog.E("Error applying CR", err)
		return nil, err
	}

	return NoContentResponse, nil
}

func applyCR(crBase64Encoded string) error {
	crBytes, err := base64.StdEncoding.DecodeString(crBase64Encoded)
	if err != nil {
		roslog.E("Error decoding base64 encoded CR file", err)
		return err
	}

	return k8s.ApplyYamlResourceFile(crBytes)
}
