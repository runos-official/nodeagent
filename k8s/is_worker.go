package k8s

import (
	"encoding/json"
	"os"

	"github.com/runos-official/nodeagent/roslog"
)

// Taint represents a Kubernetes node taint
type Taint struct {
	Key    string `json:"key"`
	Effect string `json:"effect"`
}

// Node represents a simplified Kubernetes node
type Node struct {
	Spec struct {
		Taints []Taint `json:"taints"`
	} `json:"spec"`
}

// IsWorker checks if the current node can run regular workloads
// by examining node taints
func IsWorker() bool {
	hostname, err := os.Hostname()
	if err != nil {
		roslog.E("Error getting hostname", err)
		return false
	}

	output, err := kubectlWithTimeout("get", "node", hostname, "-o", "json")
	if err != nil {
		roslog.E("Error running kubectl command", err)
		return false
	}

	var node Node
	if err := json.Unmarshal(output, &node); err != nil {
		roslog.E("Error parsing kubectl output", err)
		return false
	}

	// Check for NoSchedule taints that would prevent workloads
	for _, taint := range node.Spec.Taints {
		if (taint.Key == "node-role.kubernetes.io/control-plane" ||
			taint.Key == "node-role.kubernetes.io/master") &&
			taint.Effect == "NoSchedule" {
			return false
		}
	}

	return true
}

// GetStatus returns the current node's Kubernetes readiness status string.
func GetStatus() string {
	hostname, err := os.Hostname()
	if err != nil {
		roslog.E("Error getting hostname", err)
		return "not_ready"
	}

	output, err := kubectlWithTimeout("get", "node", hostname, "-o", "json")
	if err != nil {
		roslog.E("Error running kubectl command", err)
		return "not_ready"
	}

	var nodeData struct {
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}

	if err := json.Unmarshal(output, &nodeData); err != nil {
		roslog.E("Error parsing kubectl output", err)
		return "not_ready"
	}

	if nodeData.Spec.Unschedulable {
		return "cordoned"
	}

	for _, condition := range nodeData.Status.Conditions {
		if condition.Type == "Ready" {
			if condition.Status == "True" {
				return "ready"
			} else {
				return "not_ready"
			}
		}
	}

	return "not_ready"
}
