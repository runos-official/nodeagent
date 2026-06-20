package agentstream

import (
	"context"
	"os"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/k8s"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/version"
	"github.com/runos-official/nodeagent/vip"
)

// vipSelfDropThreshold is the number of consecutive heartbeat failures after
// which we defensively drop the VIP from wg0. Prevents a partitioned node
// from holding the VIP while Nodeward re-elects.
const vipSelfDropThreshold = 2

// NodeAgentHeartbeat sends a single heartbeat to Nodeward with the node's
// current role, status and version, and returns any send/decode error.
func NodeAgentHeartbeat() error {
	type HeartbeatRequest struct {
		ExternalIpAddress string `json:"externalIpAddress"`
		IsCp              bool   `json:"isCp"`
		IsWorker          bool   `json:"isWorker"`
		Status            string `json:"status"`
		Version           string `json:"version"`
	}

	roslog.D("Sending heartbeat to nodeagent")

	externalIp, err := commons.GetExternalIPAddress()
	if err != nil {
		roslog.E("Error getting external IP address", err)
		externalIp = ""
	}

	var isCp bool
	var isWorker bool
	var status string

	if k8s.IsInstalled() {
		isCp = k8s.IsCP()
		isWorker = k8s.IsWorker()
		status = k8s.GetStatus()
	} else {
		isCp = false
		isWorker = false
		status = "not_installed"
	}

	heartbeatRequestJsonB64, err := commons.JSONB64Encode(HeartbeatRequest{
		ExternalIpAddress: externalIp,
		IsCp:              isCp,
		IsWorker:          isWorker,
		Status:            status,
		Version:           version.Version,
	})

	if err != nil {
		roslog.E("Error encoding heartbeat request", err)
		return err
	}

	requestMsg := &l2sec.FromNodeAgent{
		JsonB64: heartbeatRequestJsonB64, // No data needed
		Type:    "NodeAgentHeartbeatToServer",
	}

	// Send the request and wait for a response with a timeout
	response, err := SendAndWaitForResponseWithTimeout(requestMsg, 10*time.Second)
	if err != nil {
		roslog.E("Error communicating with RunOS servers", err)
		return err
	}

	type HeartbeatResponse struct {
		IsHealthy bool   `json:"isHealthy"`
		Message   string `json:"message"`
	}

	var heartbeatResponse HeartbeatResponse

	if err := commons.JSONB64Decode(response.JsonB64, &heartbeatResponse); err != nil {
		roslog.E("Error decoding heartbeat response", err)
		return err
	}

	if !heartbeatResponse.IsHealthy {
		roslog.W("NodeAgentHeartbeat response is not healthy", nil, "message", heartbeatResponse.Message)
		return nil
	}

	return nil
}

func rebootDetector() {
	const rebootFlagPath = "/tmp/node_agent_running"

	// Check if the file exists
	_, err := os.Stat(rebootFlagPath)
	if os.IsNotExist(err) {
		if err := backend.AddNodelog(2, "RebootDetected", "A system reboot or startup has been detected"); err != nil {
			roslog.E("Error adding nodelog", err)
		}

		// Create the file to mark that we're running
		if err := os.WriteFile(rebootFlagPath, []byte(time.Now().String()), 0644); err != nil {
			roslog.E("Error creating reboot detector file", err)
		}
	}
}

// StartNodeAgentHeartbeatManager runs the heartbeat loop on a 5s ticker until
// ctx is cancelled, dropping the VIP and ultimately returning after too many
// consecutive failures. It returns a channel closed when the loop exits.
func StartNodeAgentHeartbeatManager(ctx context.Context) chan struct{} {
	done := make(chan struct{})

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		defer close(done)

		roslog.I("Starting agent heartbeat manager")

		consecutiveFailures := 0
		const maxConsecutiveFailures = 10

		for {
			select {
			case <-ticker.C:
				rebootDetector()
				if err := NodeAgentHeartbeat(); err != nil {
					consecutiveFailures++
					roslog.E("Heartbeat error", err, "consecutive_failures", consecutiveFailures)
					if consecutiveFailures >= vipSelfDropThreshold {
						if dropErr := vip.ForceDrop(); dropErr != nil {
							roslog.E("VIP self-drop failed", dropErr, "consecutive_failures", consecutiveFailures)
						}
					}
					if consecutiveFailures >= maxConsecutiveFailures {
						roslog.W("Max consecutive heartbeat failures reached, triggering agent restart", nil, "failures", consecutiveFailures)
						return
					}
				} else {
					consecutiveFailures = 0
				}
			case <-ctx.Done():
				roslog.I("Heartbeat manager shutting down")
				return
			}
		}
	}()

	return done
}
