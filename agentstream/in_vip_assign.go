package agentstream

import (
	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/vip"
)

const (
	// VipAssignRequestType is the instruction type that assigns the VIP to this node.
	VipAssignRequestType = "VIP_ASSIGN"
	// VipAssignResponseType is the response type acknowledging a VIP assignment.
	VipAssignResponseType = "VIP_ASSIGN_RESPONSE"
)

type vipInstructionPayload struct {
	Generation int64 `json:"generation"`
}

type vipAckPayload struct {
	Ok      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// HandleVipAssign converges wg0 to "VIP bound". On failure it returns
// (nil, nil) to signal the dispatcher that no ack should be sent — per the
// Nodeward contract, a missing ack after the 5s timeout triggers hand-off to
// the next candidate.
func HandleVipAssign(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	var req vipInstructionPayload
	if err := commons.JSONB64Decode(instruction.JsonB64, &req); err != nil {
		roslog.E("Error decoding VIP_ASSIGN payload", err)
		return nil, nil
	}

	if _, err := vip.Apply(true, req.Generation, true); err != nil {
		roslog.E("VIP_ASSIGN reconcile failed, dropping ack", err, "generation", req.Generation)
		return nil, nil
	}

	ack, err := commons.JSONB64Encode(vipAckPayload{Ok: true})
	if err != nil {
		roslog.E("Error encoding VIP_ASSIGN ack", err)
		return nil, nil
	}

	return &pb.FromNodeAgent{
		JsonB64: ack,
		Type:    VipAssignResponseType,
	}, nil
}
