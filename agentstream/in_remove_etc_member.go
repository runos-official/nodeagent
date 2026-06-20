package agentstream

import (
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/k8s"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// RemoveEtcdMemberRequestType is the instruction type that removes an etcd member.
const RemoveEtcdMemberRequestType = "REMOVE_ETCD_MEMBER"

// HandleRemoveEtcdMember decodes a REMOVE_ETCD_MEMBER instruction and removes the
// etcd member matching the given node IP.
func HandleRemoveEtcdMember(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleRemoveEtcdMember")

	type requestType struct {
		NodeIP string `json:"node_ip"`
	}
	var request requestType
	if err := commons.JSONB64Decode(instruction.JsonB64, &request); err != nil {
		roslog.E("Error decoding request data", err)
		return nil, err
	}

	// Remove the etcd member using the helper function
	if err := k8s.RemoveEtcdMemberViaEtcdCtlByIP(request.NodeIP); err != nil {
		roslog.E("Error removing etcd member", err)
		return nil, err
	}

	return NoContentResponse, nil
}
