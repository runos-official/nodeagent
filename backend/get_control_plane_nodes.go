package backend

import (
	"context"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"time"
)

// GetControlPlaneNodes fetches the current control plane node list from Nodeward.
func GetControlPlaneNodes() ([]*pb.GetControlPlaneNodesResponse_ControlPlaneNode, error) {
	c, _, backendCancel, conn, err := NodewardL2Sec()
	if err != nil {
		return nil, err
	}
	defer backendCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer conn.Close()

	request := &pb.GetControlPlaneNodesRequest{}

	roslog.I("Getting control plane nodes", "request", request)

	response, err := c.GetControlPlaneNodes(ctx, request)
	if err != nil {
		roslog.E("Error updating status", err)
		return nil, err
	}

	roslog.I("Received control plane nodes response", "response", response)

	return response.ControlPlaneNodes, nil
}
