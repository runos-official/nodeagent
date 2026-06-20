package k8s

import (
	"context"
	"os"
	"os/exec"
	"time"
)

// IsInstalled reports whether Kubernetes appears to be installed on this node.
func IsInstalled() bool {
	// Check if kubectl binary exists
	if _, err := os.Stat("/usr/bin/kubectl"); os.IsNotExist(err) {
		return false
	}

	// Check if kubeadm binary exists
	if _, err := os.Stat("/usr/bin/kubeadm"); os.IsNotExist(err) {
		return false
	}

	// Check if kubelet binary exists
	if _, err := os.Stat("/usr/bin/kubelet"); os.IsNotExist(err) {
		return false
	}

	// Check if kubernetes config directory exists
	if _, err := os.Stat("/etc/kubernetes"); os.IsNotExist(err) {
		return false
	}

	// Check if kubelet data directory exists
	if _, err := os.Stat("/var/lib/kubelet"); os.IsNotExist(err) {
		return false
	}

	// Check if kubelet service exists and is enabled (with timeout)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "is-enabled", "kubelet")
	err := cmd.Run()
	if err != nil {
		return false
	}

	return true
}
