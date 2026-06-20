package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/k8s"
)

// InstallHelmChartType is the instruction type that installs a Helm chart.
const InstallHelmChartType = "INSTALL_HELM_CHART"

type installHelmChartRequest struct {
	RepoUrl     string `json:"repoUrl"`
	MyRepoName  string `json:"myRepoName"`
	MyChartName string `json:"myChartName"`
	ChartName   string `json:"chartName"`
	Namespace   string `json:"namespace"`
	ValuesUrl   string `json:"valuesUrl"`
	Version     string `json:"version"`
}

type installHelmChartResponse struct {
	Response string `json:"response"`
}

// HandleInstallHelmChart decodes an INSTALL_HELM_CHART instruction and installs
// the requested chart, returning the command output.
func HandleInstallHelmChart(b64ScriptData *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleInstallHelmChart")
	jsonData, err := base64.StdEncoding.DecodeString(b64ScriptData.JsonB64)
	if err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	var request installHelmChartRequest
	if err := json.Unmarshal(jsonData, &request); err != nil {
		roslog.E("Error unmarshalling JSON payload", err)
		return nil, err
	}

	commandResponse, err := k8s.InstallHelmChart(
		request.RepoUrl,
		request.MyRepoName,
		request.MyChartName,
		request.ChartName,
		request.Namespace,
		request.ValuesUrl,
		request.Version,
	)

	if err != nil {
		roslog.E("Error executing helm command", err, "output", commandResponse)
		response := installHelmChartResponse{
			Response: fmt.Sprintf("%s\n\nOutput:\n%s", err.Error(), commandResponse),
		}
		responseJson, _ := json.Marshal(response)
		responseJsonB64 := base64.StdEncoding.EncodeToString(responseJson)
		return &pb.FromNodeAgent{
			JsonB64: responseJsonB64,
			Type:    "INSTALL_HELM_CHART_ERROR",
		}, nil
	}

	response := installHelmChartResponse{
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
		Type:    "INSTALL_HELM_CHART_RESPONSE",
	}, nil
}
