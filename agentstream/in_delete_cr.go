package agentstream

import (
	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/k8s"
)

// DeleteCRRequestType is the instruction type that deletes a Kubernetes custom resource.
const DeleteCRRequestType = "DELETE_CR"

type deleteCRRequest struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// HandleDeleteCR decodes a DELETE_CR instruction and deletes the referenced
// custom resource from the cluster.
func HandleDeleteCR(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleDeleteCR")

	var request deleteCRRequest
	if err := commons.JSONB64Decode(instruction.JsonB64, &request); err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	if err := k8s.DeleteCR("runos.com", "v1", request.Type, request.ID, request.ID); err != nil {
		roslog.E("Error deleting CR", err)
		return nil, err
	}

	return NoContentResponse, nil
}
