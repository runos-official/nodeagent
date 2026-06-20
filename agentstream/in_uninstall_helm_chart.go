package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/k8s"
)

// UninstallHelmChartType is the instruction type that uninstalls a Helm chart.
const UninstallHelmChartType = "UNINSTALL_HELM_CHART"

// HandleUninstallHelmChart decodes an UNINSTALL_HELM_CHART instruction and
// uninstalls the requested release, returning the command output.
func HandleUninstallHelmChart(b64ScriptData *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	type requestType struct {
		MyRepoName  string `json:"myRepoName"`
		MyChartName string `json:"myChartName"`
		Namespace   string `json:"namespace"`
	}

	roslog.I("Executing HandleUninstallHelmChart")
	jsonData, err := base64.StdEncoding.DecodeString(b64ScriptData.JsonB64)
	if err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	var request requestType
	if err = json.Unmarshal(jsonData, &request); err != nil {
		roslog.E("Error unmarshalling JSON payload", err)
		return nil, err
	}

	commandResponse, err := k8s.UninstallHelmChart(
		request.MyRepoName,
		request.MyChartName,
		request.Namespace,
	)

	type responseType struct {
		Response string `json:"response"`
	}

	if err != nil {
		roslog.E("Error executing helm command", err, "output", commandResponse)
		response := responseType{
			Response: fmt.Sprintf("%s\n\nOutput:\n%s", err.Error(), commandResponse),
		}
		responseJson, _ := json.Marshal(response)
		responseJsonB64 := base64.StdEncoding.EncodeToString(responseJson)
		return &pb.FromNodeAgent{
			JsonB64: responseJsonB64,
			Type:    "UNINSTALL_HELM_CHART_ERROR",
		}, nil
	}

	response := responseType{
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
		Type:    "UNINSTALL_HELM_CHART_RESPONSE",
	}, nil
}
