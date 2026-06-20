package k8s

import (
	"os"
	"strings"

	"github.com/runos-official/nodeagent/roslog"
)

// IsCP reports whether the current node is a Kubernetes control plane node,
// by examining its node labels for the control-plane role.
func IsCP() bool {
	hostname, err := os.Hostname()
	if err != nil {
		roslog.E("Error getting hostname", err)
		return false
	}

	output, err := kubectlWithTimeout("get", "node", hostname, "-o", "jsonpath={.metadata.labels}")
	if err != nil {
		roslog.E("Error running kubectl command", err)
		return false
	}

	labels := string(output)

	return strings.Contains(labels, "node-role.kubernetes.io/control-plane") ||
		strings.Contains(labels, "node-role.kubernetes.io/master")
}
