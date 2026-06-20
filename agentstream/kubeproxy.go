package agentstream

import (
	"context"
	"fmt"
	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/roslog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Configuration options for HAProxy
var (
	haproxyConfigPath   = "/etc/haproxy/haproxy.cfg"
	controlPlaneNodes   []string
	controlPlaneNodesMu sync.RWMutex
	// Interval for checking control plane nodes updates
	nodeCheckInterval = 5 * time.Second
)

// InitControlPlaneNodesRegistry initializes the registry for control plane nodes
func InitControlPlaneNodesRegistry() {
	controlPlaneNodes = make([]string, 0)
}

// StartHAProxyServer ensures HAProxy is properly configured for Kubernetes API proxy
// Returns a channel that will be closed when the server monitoring is done
func StartHAProxyServer(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		// Set initial control plane nodes if available
		initialNodes := GetControlPlaneNodes()
		if len(initialNodes) > 0 {
			if err := updateHAProxyConfig(initialNodes); err != nil {
				roslog.E("Error setting initial HAProxy control plane nodes", err)
			}
		} else {
			// If we don't have any initial nodes, try to request them from Nodeward
			requestControlPlaneNodesFromNodeward()
		}

		roslog.I("HAProxy configured with hosts", "hosts", controlPlaneNodes)

		// Start continuous monitoring of control plane nodes
		monitorControlPlaneNodes(ctx)
	}()

	return done
}

// updateHAProxyConfig updates the HAProxy config file with the current control plane nodes
func updateHAProxyConfig(nodes []string) error {
	// Safety check - don't update config if node list is empty
	if len(nodes) == 0 {
		roslog.W("Empty control plane node list. Keeping existing HAProxy configuration.", nil)
		return nil
	}

	// Ensure the haproxy directory exists
	haproxyDir := "/etc/haproxy"
	if err := os.MkdirAll(haproxyDir, 0755); err != nil {
		return fmt.Errorf("failed to create haproxy directory: %w", err)
	}

	// Generate a new HAProxy configuration
	config := `global
    log /dev/log local0
    log /dev/log local1 notice
    user haproxy
    group haproxy
    daemon

defaults
    log global
    mode tcp
    option tcplog
    option dontlognull
    timeout connect 5000
    timeout client 50000
    timeout server 50000

frontend k8s_api
    bind *:6446
    mode tcp
    default_backend k8s_api_backend

backend k8s_api_backend
    mode tcp
`

	// Add each server to the config
	for i, node := range nodes {
		// Parse the URL to extract host and port
		parts := strings.Split(strings.TrimPrefix(node, "https://"), ":")
		if len(parts) != 2 {
			roslog.W("Invalid node URL format", nil, "node", node)
			continue
		}

		host := parts[0]
		port := parts[1]
		serverName := fmt.Sprintf("api-%d", i+1)

		config += fmt.Sprintf("    server %s %s:%s check\n", serverName, host, port)
	}

	// Write the new config to a temporary file
	tempFile := haproxyConfigPath + ".new"
	if err := os.WriteFile(tempFile, []byte(config), 0644); err != nil {
		return fmt.Errorf("failed to write new HAProxy config: %w", err)
	}

	// Move the temporary file to the real location
	if err := os.Rename(tempFile, haproxyConfigPath); err != nil {
		return fmt.Errorf("failed to update HAProxy config: %w", err)
	}

	// Reload HAProxy
	if err := exec.Command("systemctl", "reload", "haproxy").Run(); err != nil {
		return fmt.Errorf("failed to reload HAProxy: %w", err)
	}

	roslog.I("HAProxy configuration updated", "node_count", len(nodes))
	return nil
}

// monitorControlPlaneNodes continuously polls for control plane node updates
func monitorControlPlaneNodes(ctx context.Context) {
	ticker := time.NewTicker(nodeCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			requestControlPlaneNodesFromNodeward()
		case <-ctx.Done():
			roslog.I("Stopping control plane nodes monitoring...")
			return
		}
	}
}

// requestControlPlaneNodesFromNodeward asks Nodeward for the current list of control plane nodes
func requestControlPlaneNodesFromNodeward() {
	cpNodes, err := GetCPNodes()
	//log.Printf("Found %d cp nodes: %v", len(cpNodes), cpNodes)
	if err != nil {
		roslog.E("Error getting control plane nodes", err)
		return
	}

	// Check if the nodes list has changed before updating
	currentNodes := GetControlPlaneNodes()
	if !equalStringSlices(currentNodes, cpNodes) {
		// Update the control plane nodes only if there's a change
		SetControlPlaneNodes(cpNodes)
		roslog.I("Updated control plane nodes from Nodeward", "nodes", cpNodes)
	} else {
		//log.Printf("Control plane nodes unchanged: %v", cpNodes)
	}
}

// equalStringSlices compares two string slices for equality
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	// Create maps for quick lookup
	mapA := make(map[string]bool)
	for _, val := range a {
		mapA[val] = true
	}

	// Check if all elements in b exist in a
	for _, val := range b {
		if !mapA[val] {
			return false
		}
	}

	return true
}

// SetControlPlaneNodes updates the list of available control plane nodes
func SetControlPlaneNodes(nodes []string) {
	// Safety check - don't update with empty list
	if len(nodes) == 0 {
		roslog.W("Attempted to set empty control plane node list. Ignoring update.", nil)
		return
	}

	controlPlaneNodesMu.Lock()
	defer controlPlaneNodesMu.Unlock()

	// Make a copy to avoid race conditions
	controlPlaneNodes = make([]string, len(nodes))
	copy(controlPlaneNodes, nodes)

	// Update HAProxy configuration
	if err := updateHAProxyConfig(nodes); err != nil {
		roslog.E("Error updating HAProxy control plane nodes", err)
	}
}

// GetControlPlaneNodes returns a copy of the current control plane nodes list
func GetControlPlaneNodes() []string {
	controlPlaneNodesMu.RLock()
	defer controlPlaneNodesMu.RUnlock()

	result := make([]string, len(controlPlaneNodes))
	copy(result, controlPlaneNodes)
	return result
}

// ForceKubeproxyUpdate forces an update of the HAProxy configuration
// by fetching fresh control plane nodes directly from Nodeward via L2Sec
// instead of using the agent stream
func ForceKubeproxyUpdate() error {
	roslog.I("Forcing kube-proxy update...")

	// Get control plane nodes directly from Nodeward using the backend package
	cpNodeObjects, err := backend.GetControlPlaneNodes()
	if err != nil {
		roslog.E("Error fetching control plane nodes from Nodeward", err)
		return err
	}

	if len(cpNodeObjects) == 0 {
		roslog.I("No control plane nodes found from Nodeward")
		return nil
	}

	// Convert the node objects to string endpoints for HAProxy
	endpoints := make([]string, 0, len(cpNodeObjects))
	for _, node := range cpNodeObjects {
		endpoints = append(endpoints, node.KubeEndpoint)
	}

	// Force update the HAProxy config with the fresh nodes
	SetControlPlaneNodes(endpoints)
	roslog.I("HAProxy configuration forcefully updated with %d control plane nodes "+
		"from Nodeward with values %v", len(endpoints), endpoints)

	// Wait briefly to ensure HAProxy has time to reload
	time.Sleep(1 * time.Second)

	return nil
}
