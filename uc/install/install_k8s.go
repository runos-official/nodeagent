package install

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// K8s installs Kubernetes on this node by fetching and running the install
// command list from Nodeward.
func K8s() error {
	c, _, backendCancel, conn, err := backend.NodewardL2Sec()
	if err != nil {
		return err
	}
	// The backend hands back its own cancel; defer it before reassigning so the
	// original context is not leaked when we replace ctx/cancel below.
	defer backendCancel()

	// Extend the timeout, because this is a stream.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	defer cancel()
	defer conn.Close()

	var fileList map[string]string

	// Get all files in /etc/netplan
	fileList, err = commons.GetAllFilesInDirectory("/etc/netplan")
	if err != nil {
		roslog.E("Error getting files in /etc/netplan", err)
	}

	// Encode the file list to JSON and then to base64
	fileListEncoded, err := commons.JSONB64Encode(fileList)

	request := &pb.GetInstallCommandsRequest{
		JsonB64FileList: fileListEncoded,
	}

	// Retry logic for transient "node agent not connected" errors
	// This can happen during cloud-init when there's a timing issue with stream connectivity
	var res *pb.InstallCommandList
	maxRetries := 5
	retryDelay := 2 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		res, err = c.GetInstallCommands(ctx, request)
		if err == nil {
			break
		}

		// Check if this is a transient "not connected" error
		if strings.Contains(err.Error(), "node agent") && strings.Contains(err.Error(), "is not connected") {
			if attempt < maxRetries {
				roslog.W("Node agent stream not ready, retrying", err, "attempt", attempt, "delay", retryDelay)
				time.Sleep(retryDelay)
				retryDelay *= 2 // Exponential backoff
				continue
			}
		}

		// Non-retryable error or max retries exceeded. Return a contextual error
		// (never panic) so the install exits non-zero with an actionable message
		// instead of dumping a Go stack trace under the systemd service.
		roslog.E("Error executing GetInstallCommands", err, "attempt", attempt)
		return fmt.Errorf("could not fetch install commands from Nodeward after %d attempts: %w (check connectivity to Nodeward operations channel on TCP 9192 and that the node is registered)", maxRetries, err)
	}

	return commons.ProcessInstallCommandsStatusAware(res)
}
