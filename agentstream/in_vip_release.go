package agentstream

import (
	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/vip"
)

const (
	// VipReleaseRequestType is the instruction type that releases the VIP from this node.
	VipReleaseRequestType = "VIP_RELEASE"
	// VipReleaseResponseType is the response type acknowledging a VIP release.
	VipReleaseResponseType = "VIP_RELEASE_RESPONSE"
)

// HandleVipRelease converges wg0 to "VIP unbound". Unlike VIP_ASSIGN, this
// handler always returns an ack (even on failure) — Nodeward only waits 3s and
// proceeds regardless, so the ack is best-effort status for telemetry.
func HandleVipRelease(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	var req vipInstructionPayload
	if err := commons.JSONB64Decode(instruction.JsonB64, &req); err != nil {
		roslog.E("Error decoding VIP_RELEASE payload", err)
		ack, _ := commons.JSONB64Encode(vipAckPayload{Ok: false, Message: err.Error()})
		return &pb.FromNodeAgent{JsonB64: ack, Type: VipReleaseResponseType}, nil
	}

	ackPayload := vipAckPayload{Ok: true}
	if _, err := vip.Apply(false, req.Generation, true); err != nil {
		roslog.E("VIP_RELEASE reconcile failed", err, "generation", req.Generation)
		ackPayload.Ok = false
		ackPayload.Message = err.Error()
	}

	ack, err := commons.JSONB64Encode(ackPayload)
	if err != nil {
		roslog.E("Error encoding VIP_RELEASE ack", err)
		return nil, err
	}

	return &pb.FromNodeAgent{
		JsonB64: ack,
		Type:    VipReleaseResponseType,
	}, nil
}
