package backend

import (
	"context"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"time"
)

// UpdateStatus reports the node's current lifecycle status to Nodeward.
func UpdateStatus(status string) error {
	c, _, backendCancel, conn, err := NodewardL2Sec()
	if err != nil {
		return err
	}
	defer backendCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer conn.Close()

	request := &pb.UpdateStatusRequest{
		Status: status,
	}

	roslog.I("Updating status", "request", request)

	response, err := c.UpdateStatus(ctx, request)
	if err != nil {
		roslog.E("Error updating status", err)
		return err
	}

	roslog.I("Received UpdateStatus response", "response", response)

	return nil
}
