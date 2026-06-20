package agentstream

import (
	"time"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

const (
	// AmIVipHolderRequestType is the outbound query asking whether this node holds the VIP.
	AmIVipHolderRequestType = "AmIVIPHolder"
	// AmIVipHolderResponseType is the response type for the VIP-holder query.
	AmIVipHolderResponseType = "AmIVIPHolderResponse"
)

type amIVipHolderResponse struct {
	IsHolder   bool  `json:"isHolder"`
	Generation int64 `json:"generation"`
}

// QueryVipHolderStatus asks Nodeward whether this node should currently hold
// the VIP. It sends AmIVIPHolder over the NodeAgentStream and blocks until the
// AmIVIPHolderResponse arrives (or the timeout fires). The instruction stream
// handler must already be running so response correlation works.
func QueryVipHolderStatus() (bool, int64, error) {
	emptyJsonB64, err := commons.JSONB64Encode(struct{}{})
	if err != nil {
		return false, 0, err
	}

	req := &l2sec.FromNodeAgent{
		JsonB64: emptyJsonB64,
		Type:    AmIVipHolderRequestType,
	}

	resp, err := SendAndWaitForResponseWithTimeout(req, 10*time.Second)
	if err != nil {
		roslog.E("Error querying VIP holder status", err)
		return false, 0, err
	}

	var decoded amIVipHolderResponse
	if err := commons.JSONB64Decode(resp.JsonB64, &decoded); err != nil {
		roslog.E("Error decoding VIP holder response", err)
		return false, 0, err
	}

	roslog.I("VIP holder status from Nodeward",
		"is_holder", decoded.IsHolder, "generation", decoded.Generation)
	return decoded.IsHolder, decoded.Generation, nil
}
