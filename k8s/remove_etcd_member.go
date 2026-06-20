package k8s

import (
	"fmt"
	"github.com/runos-official/nodeagent/roslog"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// EtcdMemberV2 represents an etcd cluster member with extended information
type EtcdMemberV2 struct {
	ID         string
	Name       string
	PeerURLs   string
	ClientURLs string
	Started    bool
	IsLearner  bool
	IsLeader   bool
	Health     string
}

// EtcdClusterInfo represents overall cluster information
type EtcdClusterInfo struct {
	Members     []EtcdMemberV2
	LeaderID    string
	LeaderName  string
	ClusterID   string
	Revision    string
	DbSizeBytes int64
	IsHealthy   bool
}

// RemoveEtcdMemberDirect safely removes an etcd member using direct etcdctl commands
func RemoveEtcdMemberDirect(memberID string) error {
	roslog.I("Removing etcd member", "memberId", memberID)

	// Step 1: Verify the member exists and get current member list
	members, err := listEtcdMembersDirect()
	if err != nil {
		return fmt.Errorf("failed to list etcd members: %v", err)
	}

	// Check if member exists
	var targetMember *EtcdMemberV2
	for _, member := range members {
		if member.ID == memberID {
			targetMember = &member
			break
		}
	}

	if targetMember == nil {
		roslog.I("Member not found, may already be removed", "memberId", memberID)
		return nil // Member doesn't exist, consider it successfully removed
	}

	// Step 2: Verify cluster health before removal
	healthy, err := checkEtcdHealthDirect()
	if err != nil {
		return fmt.Errorf("failed to check etcd health: %v", err)
	}
	if !healthy {
		return fmt.Errorf("etcd cluster is not healthy, refusing to remove member")
	}

	// Step 3: Check if removal would break quorum
	if len(members) <= 1 {
		return fmt.Errorf("cannot remove member: only %d member(s) remain", len(members))
	}

	roslog.I("Removing etcd member", "id", memberID, "name", targetMember.Name)

	// Step 4: Execute the removal command using direct etcdctl
	cmd := exec.Command("etcdctl",
		"--endpoints=https://127.0.0.1:2379",
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/peer.crt",
		"--key=/etc/kubernetes/pki/etcd/peer.key",
		"member", "remove", memberID)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to remove etcd member: %v, output: %s", err, string(output))
	}

	roslog.I("Member removal command executed", "output", string(output))

	// Step 5: Wait for removal to propagate and verify
	maxRetries := 30 // 30 seconds max wait
	for i := 0; i < maxRetries; i++ {
		time.Sleep(1 * time.Second)

		// Check if member is gone
		currentMembers, err := listEtcdMembersDirect()
		if err != nil {
			roslog.W("Failed to list members during verification", err)
			continue
		}

		memberGone := true
		for _, member := range currentMembers {
			if member.ID == memberID {
				memberGone = false
				break
			}
		}

		if memberGone {
			roslog.I("Member successfully removed and verified", "memberId", memberID)
			return nil
		}
	}

	// Final verification failed
	return fmt.Errorf("member removal completed but verification failed after 30 seconds")
}

// GetEtcdClusterInfo returns comprehensive etcd cluster information
func GetEtcdClusterInfo() (*EtcdClusterInfo, error) {
	members, err := listEtcdMembersDirectExtended()
	if err != nil {
		return nil, fmt.Errorf("failed to list etcd members: %v", err)
	}

	// Get leader information
	leaderID, err := getEtcdLeader()
	if err != nil {
		roslog.W("Failed to get etcd leader", err)
	}

	// Get cluster status
	clusterID, revision, dbSize, err := getEtcdStatus()
	if err != nil {
		roslog.W("Failed to get etcd status", err)
	}

	// Check overall health
	healthy, _ := checkEtcdHealthDirect()

	// Mark leader in members and get leader name
	var leaderName string
	for i := range members {
		if members[i].ID == leaderID {
			members[i].IsLeader = true
			leaderName = members[i].Name
		}
		// Get individual member health
		members[i].Health = getMemberHealth(members[i].ClientURLs)
	}

	return &EtcdClusterInfo{
		Members:     members,
		LeaderID:    leaderID,
		LeaderName:  leaderName,
		ClusterID:   clusterID,
		Revision:    revision,
		DbSizeBytes: dbSize,
		IsHealthy:   healthy,
	}, nil
}

// listEtcdMembersDirect returns the current list of etcd members using direct etcdctl
func listEtcdMembersDirect() ([]EtcdMemberV2, error) {
	cmd := exec.Command("etcdctl",
		"--endpoints=https://127.0.0.1:2379",
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/peer.crt",
		"--key=/etc/kubernetes/pki/etcd/peer.key",
		"member", "list", "-w", "table")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list etcd members: %v", err)
	}

	return parseEtcdMembersDirect(string(output))
}

// listEtcdMembersDirectExtended returns detailed etcd member information
func listEtcdMembersDirectExtended() ([]EtcdMemberV2, error) {
	cmd := exec.Command("etcdctl",
		"--endpoints=https://127.0.0.1:2379",
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/peer.crt",
		"--key=/etc/kubernetes/pki/etcd/peer.key",
		"member", "list", "-w", "table")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list etcd members: %v", err)
	}

	return parseEtcdMembersDirectExtended(string(output))
}

// parseEtcdMembersDirect parses the etcdctl member list output
func parseEtcdMembersDirect(output string) ([]EtcdMemberV2, error) {
	lines := strings.Split(output, "\n")
	var members []EtcdMemberV2

	// Skip header and separator lines
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "+--") || strings.Contains(line, "ID") {
			continue
		}

		// Parse table format: | ID | STATUS | NAME | PEER ADDRS | CLIENT ADDRS | IS LEARNER |
		parts := strings.Split(line, "|")
		if len(parts) >= 6 {
			member := EtcdMemberV2{
				ID:       strings.TrimSpace(parts[1]),
				Name:     strings.TrimSpace(parts[3]),
				PeerURLs: strings.TrimSpace(parts[4]),
				Started:  strings.TrimSpace(parts[2]) == "started",
			}
			if member.ID != "" {
				members = append(members, member)
			}
		}
	}

	return members, nil
}

// parseEtcdMembersDirectExtended parses extended etcd member information
func parseEtcdMembersDirectExtended(output string) ([]EtcdMemberV2, error) {
	lines := strings.Split(output, "\n")
	var members []EtcdMemberV2

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "+--") || strings.Contains(line, "ID") {
			continue
		}

		// Parse table format: | ID | STATUS | NAME | PEER ADDRS | CLIENT ADDRS | IS LEARNER |
		parts := strings.Split(line, "|")
		if len(parts) >= 6 {
			member := EtcdMemberV2{
				ID:         strings.TrimSpace(parts[1]),
				Name:       strings.TrimSpace(parts[3]),
				PeerURLs:   strings.TrimSpace(parts[4]),
				ClientURLs: strings.TrimSpace(parts[5]),
				Started:    strings.TrimSpace(parts[2]) == "started",
				IsLearner:  len(parts) >= 7 && strings.TrimSpace(parts[6]) == "true",
				IsLeader:   false, // Will be set later
			}
			if member.ID != "" {
				members = append(members, member)
			}
		}
	}

	return members, nil
}

func getEtcdLeader() (string, error) {
	// Get extended member info which includes ClientURLs
	members, err := listEtcdMembersDirectExtended()
	if err != nil {
		return "", fmt.Errorf("failed to list members: %v", err)
	}

	// Build endpoints list from all members
	var endpoints []string
	for _, member := range members {
		if member.ClientURLs != "" {
			endpoints = append(endpoints, member.ClientURLs)
		}
	}

	if len(endpoints) == 0 {
		return "", fmt.Errorf("no client endpoints found")
	}

	// Check status of all endpoints
	cmd := exec.Command("etcdctl",
		"--endpoints="+strings.Join(endpoints, ","),
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/peer.crt",
		"--key=/etc/kubernetes/pki/etcd/peer.key",
		"endpoint", "status", "-w", "table")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get etcd status: %v", err)
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "https://") && !strings.Contains(line, "ENDPOINT") {
			parts := strings.Split(line, "|")
			if len(parts) >= 6 {
				isLeader := strings.TrimSpace(parts[5]) == "true"
				if isLeader {
					leaderID := strings.TrimSpace(parts[2])
					return leaderID, nil
				}
			}
		}
	}

	return "", fmt.Errorf("leader not found in status output")
}

func getEtcdStatus() (clusterID, revision string, dbSize int64, err error) {
	cmd := exec.Command("etcdctl",
		"--endpoints=https://127.0.0.1:2379",
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/peer.crt",
		"--key=/etc/kubernetes/pki/etcd/peer.key",
		"endpoint", "status", "-w", "table")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to get etcd status: %v", err)
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "https://") && !strings.Contains(line, "ENDPOINT") {
			parts := strings.Split(line, "|")
			if len(parts) >= 9 {
				// Format: | ENDPOINT | ID | VERSION | DB SIZE | IS LEADER | IS LEARNER | RAFT TERM | RAFT INDEX | RAFT APPLIED INDEX | ERRORS |
				clusterID = strings.TrimSpace(parts[2]) // ID field
				revision = strings.TrimSpace(parts[8])  // RAFT INDEX

				// Parse DB size (format like "6.2 MB")
				dbSizeStr := strings.TrimSpace(parts[4])
				if strings.Contains(dbSizeStr, "MB") {
					sizeStr := strings.Fields(dbSizeStr)[0]
					if size, parseErr := strconv.ParseFloat(sizeStr, 64); parseErr == nil {
						dbSize = int64(size * 1024 * 1024) // Convert MB to bytes
					}
				} else if strings.Contains(dbSizeStr, "kB") {
					sizeStr := strings.Fields(dbSizeStr)[0]
					if size, parseErr := strconv.ParseFloat(sizeStr, 64); parseErr == nil {
						dbSize = int64(size * 1024) // Convert kB to bytes
					}
				}

				return clusterID, revision, dbSize, nil
			}
		}
	}

	return "", "", 0, fmt.Errorf("status not found in output")
}

// getMemberHealth checks individual member health
func getMemberHealth(clientURL string) string {
	if clientURL == "" {
		return "unknown"
	}

	cmd := exec.Command("etcdctl",
		"--endpoints="+clientURL,
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/peer.crt",
		"--key=/etc/kubernetes/pki/etcd/peer.key",
		"endpoint", "health")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "unhealthy"
	}

	if strings.Contains(string(output), "is healthy") {
		return "healthy"
	}
	return "unhealthy"
}

// checkEtcdHealthDirect verifies that the etcd cluster is healthy using direct etcdctl
func checkEtcdHealthDirect() (bool, error) {
	cmd := exec.Command("etcdctl",
		"--endpoints=https://127.0.0.1:2379",
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/peer.crt",
		"--key=/etc/kubernetes/pki/etcd/peer.key",
		"endpoint", "health")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("health check failed: %v, output: %s", err, string(output))
	}

	// Check if output contains "is healthy"
	return strings.Contains(string(output), "is healthy"), nil
}

// GetEtcdMemberIDByName finds an etcd member ID by node name
func GetEtcdMemberIDByName(nodeName string) (string, error) {
	members, err := listEtcdMembersDirect()
	if err != nil {
		return "", fmt.Errorf("failed to list etcd members: %v", err)
	}

	for _, member := range members {
		if member.Name == nodeName {
			return member.ID, nil
		}
	}

	return "", fmt.Errorf("etcd member not found for node: %s", nodeName)
}

// GetEtcdMemberIDByPeerURL finds an etcd member ID by peer URL (useful when name doesn't match)
func GetEtcdMemberIDByPeerURL(peerURL string) (string, error) {
	members, err := listEtcdMembersDirect()
	if err != nil {
		return "", fmt.Errorf("failed to list etcd members: %v", err)
	}

	for _, member := range members {
		if strings.Contains(member.PeerURLs, peerURL) {
			return member.ID, nil
		}
	}

	return "", fmt.Errorf("etcd member not found for peer URL: %s", peerURL)
}

// ListAllEtcdMembers returns all current etcd members (convenience function)
func ListAllEtcdMembers() ([]EtcdMemberV2, error) {
	return listEtcdMembersDirect()
}

// RemoveEtcdMemberByNodeName removes an etcd member by Kubernetes node name
func RemoveEtcdMemberByNodeName(nodeName string) error {
	memberID, err := GetEtcdMemberIDByName(nodeName)
	if err != nil {
		return fmt.Errorf("failed to find etcd member for node %s: %v", nodeName, err)
	}

	return RemoveEtcdMemberDirect(memberID)
}

// RemoveEtcdMemberViaEtcdCtlByIP removes an etcd member by node IP address
func RemoveEtcdMemberViaEtcdCtlByIP(nodeIP string) error {
	members, err := listEtcdMembersDirect()
	roslog.I("Found etcd members", "members", members)
	if err != nil {
		return fmt.Errorf("failed to list etcd members: %v", err)
	}

	for _, member := range members {
		// Check if the IP appears in the peer URLs
		if strings.Contains(member.PeerURLs, nodeIP) {
			return RemoveEtcdMemberDirect(member.ID)
		}
	}

	return fmt.Errorf("etcd member not found for IP: %s", nodeIP)
}
