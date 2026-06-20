package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/config"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// RunRemoteScriptRequestType is the instruction type that fetches and runs a remote script.
const RunRemoteScriptRequestType = "RUN_REMOTE_SCRIPT"

type runRemoteScriptRequest struct {
	Script          string            `json:"script"`
	Params          map[string]string `json:"params"`
	RunInBackground bool              `json:"runInBackground"`
}

type runRemoteScriptResponse struct {
	Response string `json:"response"`
}

// HandleRunRemoteScript fetches a script from the configured installer/conductor
// endpoint and runs it, returning the captured output (or a background marker).
func HandleRunRemoteScript(b64ScriptData *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleRunRemoteScript")
	jsonData, err := base64.StdEncoding.DecodeString(b64ScriptData.JsonB64)
	if err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	var request runRemoteScriptRequest
	if err := json.Unmarshal(jsonData, &request); err != nil {
		roslog.E("Error unmarshalling JSON payload", err)
		return nil, err
	}

	var url string
	if strings.HasPrefix(request.Script, "/t/") {
		url = config.GetConductorURL() + request.Script
	} else {
		jsonBytes, err := json.Marshal(request.Params)
		if err != nil {
			roslog.E("Error marshalling JSON payload", err)
			return nil, err
		}
		paramsB64 := base64.StdEncoding.EncodeToString(jsonBytes)
		url = config.GetROSInstallerURL() + "/scripts?t=" + request.Script + "&i=" + paramsB64
	}

	var commandResponse string
	if request.RunInBackground {
		err = commons.ExecuteCommandInDetachedSystemdScope("curl -sSL \"" + url + "\" | bash")
		if err != nil {
			return nil, err
		}
		commandResponse = "Script is running in the background."
	} else {
		commandResponse = commons.ExecuteCommandGetResponse("curl -sSL \"" + url + "\" | bash")
	}

	// Log only metadata; the script's captured output may contain secrets and
	// must not be persisted to the on-disk log.
	roslog.I("Script result", "bytes", len(commandResponse))

	response := runRemoteScriptResponse{
		Response: commandResponse,
	}

	responseJson, err := json.Marshal(response)
	if err != nil {
		roslog.E("Error marshalling response JSON", err)
		return nil, err
	}
	responseJsonB64 := base64.StdEncoding.EncodeToString(responseJson)

	return &pb.FromNodeAgent{
		JsonB64: responseJsonB64,
		Type:    "RUN_REMOTE_SCRIPT_RESPONSE",
	}, nil
}
