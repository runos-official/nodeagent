package agentstream

import (
	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/k8s"
)

// RunKubectlCommandRequestType is the instruction type that runs a kubectl command.
const RunKubectlCommandRequestType = "RUN_KUBECTL_COMMAND"

type runKubectlCommandResponse struct {
	Response string `json:"response"`
}

// HandleRunKubectlCommand decodes a RUN_KUBECTL_COMMAND instruction (a list of
// kubectl arguments), executes it, and returns the captured output.
func HandleRunKubectlCommand(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleRunKubectlCommand")

	var request []string
	if err := commons.JSONB64Decode(instruction.JsonB64, &request); err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	executeResponse, err := k8s.RunKubectlCommand(request)
	if err != nil {
		roslog.E("Error executing kubectl command", err)
		return nil, err
	}

	responseJsonB64, err := commons.JSONB64Encode(runKubectlCommandResponse{
		Response: executeResponse,
	})
	if err != nil {
		roslog.E("Error marshalling response JSON", err)
		return nil, err
	}

	return &pb.FromNodeAgent{
		JsonB64: responseJsonB64,
		Type:    "KUBECTL_COMMAND_RESPONSE",
	}, nil
}
