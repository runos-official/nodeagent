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
)

// heartbeatRequest is the wire payload of a node agent heartbeat. Nodeward
// decodes exactly these field names in instructions/node_agent_heartbeat.go, so
// a rename here silently changes what Nodeward stores.
//
// RolesKnown tells Nodeward whether isCp/isWorker come from a fresh read of the
// node object (true) or are carried from the last known values (false). Nodeward
// must not demote a node on a heartbeat whose roles are carried. An agent older
// than v1.8.0-rc.16 omits the field, and Nodeward reads an absent field as true,
// which is the behaviour those agents already had.
type heartbeatRequest struct {
	ExternalIpAddress string `json:"externalIpAddress"`
	IsCp              bool   `json:"isCp"`
	IsWorker          bool   `json:"isWorker"`
	Status            string `json:"status"`
	Version           string `json:"version"`
	RolesKnown        bool   `json:"rolesKnown"`
}

// NodeAgentHeartbeat sends a single heartbeat to Nodeward with the node's
// current role, status and version, and returns any send/decode error.
func NodeAgentHeartbeat() error {
	roslog.D("Sending heartbeat to nodeagent")

	externalIp, err := commons.GetExternalIPAddress()
	if err != nil {
		roslog.E("Error getting external IP address", err)
		externalIp = ""
	}

	var isCp bool
	var isWorker bool
	var status string
	var rolesKnown bool

	if k8s.IsInstalled() {
		snapshot := k8s.NodeRoleSnapshot()
		isCp = snapshot.IsCp
		isWorker = snapshot.IsWorker
		status = snapshot.Status
		rolesKnown = snapshot.RolesKnown
	} else {
		// Kubernetes is not installed. That IS a fresh, local fact, so the roles
		// are known: this node holds no role.
		isCp = false
		isWorker = false
		status = "not_installed"
		rolesKnown = true
	}

	heartbeatRequestJsonB64, err := commons.JSONB64Encode(heartbeatRequest{
		ExternalIpAddress: externalIp,
		IsCp:              isCp,
		IsWorker:          isWorker,
		Status:            status,
		Version:           version.Version,
		RolesKnown:        rolesKnown,
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
