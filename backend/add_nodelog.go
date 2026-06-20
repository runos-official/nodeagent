package backend

import (
	"context"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"time"
)

// AddNodelog sends a node-scoped log entry to Nodeward with the given severity,
// type and message.
func AddNodelog(severity int, logType string, message string) error {
	c, _, backendCancel, conn := NodewardL2Sec()
	defer backendCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer conn.Close()

	request := &pb.AddNodelogRequest{
		Severity: int32(severity),
		Type:     logType,
		Message:  message,
	}

	roslog.I("Sending AddNodelogRequest", "request", request)

	response, err := c.AddNodelog(ctx, request)
	if err != nil {
		roslog.E("Error add nodelog", err)
		return err
	}

	roslog.I("Received AddNodelog response", "response", response)

	return nil
}
