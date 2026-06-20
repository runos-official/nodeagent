package agentstream

import (
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"time"
)

// GetCPNodes asks Nodeward for the current set of control plane node addresses.
func GetCPNodes() ([]string, error) {
	requestMsg := &l2sec.FromNodeAgent{
		JsonB64: "", // No data needed
		Type:    "GetControlPlaneNodes",
	}

	// Send the request and wait for response with a timeout
	response, err := SendAndWaitForResponseWithTimeout(requestMsg, 10*time.Second)
	if err != nil {
		roslog.E("Error requesting control plane nodes", err)
		return nil, err
	}

	var nodeList []string

	if err := commons.JSONB64Decode(response.JsonB64, &nodeList); err != nil {
		roslog.E("Error decoding control plane nodes response", err)
		return nil, err
	}

	if len(nodeList) == 0 {
		roslog.I("No control plane nodes found")
		return []string{}, nil
	}

	return nodeList, nil
}
