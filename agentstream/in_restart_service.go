package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"

	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

const (
	// RestartServiceRequestType asks the agent to restart its own systemd unit.
	RestartServiceRequestType = "RESTART_SERVICE"
	// RestartServiceResponseType acknowledges that a restart has been scheduled.
	RestartServiceResponseType = "RESTART_SCHEDULED"
)

// restartServiceDefaultDelaySeconds is how long the transient restart unit waits
// before restarting runos.service. The delay lets this handler's ACK flush over
// the instruction stream before the agent process is stopped, so Nodeward sees a
// clean "scheduled" ack rather than an abrupt disconnect it must interpret.
const restartServiceDefaultDelaySeconds = 2

// restartServiceMaxDelaySeconds bounds an operator-supplied delay.
const restartServiceMaxDelaySeconds = 60

type restartServiceRequest struct {
	// DelaySeconds optionally overrides the pre-restart delay. 0 uses the default.
	DelaySeconds int `json:"delaySeconds"`
}

type restartServiceResponse struct {
	Scheduled    bool `json:"scheduled"`
	DelaySeconds int  `json:"delaySeconds"`
}

// HandleRestartService schedules a restart of the runos systemd unit and ACKs
// immediately. The restart itself runs as a transient systemd unit (systemd-run)
// so it lives OUTSIDE runos.service's cgroup and completes even though restarting
// stops (kills) this agent process. Because the agent disconnects during the
// restart, the caller confirms success out-of-band: it watches the agent
// reconnect and the node return to Ready (Nodeward's NodeAgentOnline + heartbeat).
func HandleRestartService(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	var request restartServiceRequest
	if instruction.JsonB64 != "" {
		if jsonData, err := base64.StdEncoding.DecodeString(instruction.JsonB64); err == nil {
			// Best-effort: an empty or malformed payload just uses the defaults.
			_ = json.Unmarshal(jsonData, &request)
		}
	}

	delay := request.DelaySeconds
	if delay <= 0 {
		delay = restartServiceDefaultDelaySeconds
	}
	if delay > restartServiceMaxDelaySeconds {
		delay = restartServiceMaxDelaySeconds
	}

	if err := scheduleServiceRestart(delay); err != nil {
		roslog.E("Failed to schedule runos service restart", err)
		return nil, err
	}

	roslog.I("Scheduled runos service restart", "delaySeconds", delay)

	responseJson, err := json.Marshal(restartServiceResponse{Scheduled: true, DelaySeconds: delay})
	if err != nil {
		roslog.E("Error marshalling restart response JSON", err)
		return nil, err
	}

	return &pb.FromNodeAgent{
		JsonB64: base64.StdEncoding.EncodeToString(responseJson),
		Type:    RestartServiceResponseType,
	}, nil
}

// scheduleServiceRestart launches a transient systemd unit that waits `delay`
// seconds then restarts runos.service. systemd-run detaches the restart from
// this process's cgroup so it survives the agent being stopped. The unit name is
// auto-generated (no fixed --unit) to avoid colliding with a prior restart still
// being reaped. If systemd-run is unavailable it falls back to a detached setsid
// sleeper that enqueues a non-blocking restart.
func scheduleServiceRestart(delay int) error {
	restartCmd := fmt.Sprintf("sleep %d; systemctl restart runos.service", delay)

	if path, err := exec.LookPath("systemd-run"); err == nil {
		// --collect reaps the transient unit once it exits so it does not linger.
		cmd := exec.Command(path,
			"--collect",
			"--description", "RunOS node agent self-restart",
			"/bin/sh", "-c", restartCmd,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("systemd-run failed: %v (%s)", err, string(out))
		}
		return nil
	}

	// Fallback: detach with setsid so the sleeper survives this process exiting,
	// and use --no-block so the enqueue returns even if the sleeper is reaped when
	// the unit's cgroup is torn down.
	fallback := fmt.Sprintf("sleep %d; systemctl restart --no-block runos.service", delay)
	cmd := exec.Command("setsid", "/bin/sh", "-c", fallback)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("setsid fallback failed: %w", err)
	}
	return nil
}
