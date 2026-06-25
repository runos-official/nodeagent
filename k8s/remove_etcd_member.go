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
	ID         string `json:"id"`
	Name       string `json:"name"`
	PeerURLs   string `json:"peerUrls"`
	ClientURLs string `json:"clientUrls"`
	Started    bool   `json:"started"`
	IsLearner  bool   `json:"isLearner"`
	IsLeader   bool   `json:"isLeader"`
	Health     string `json:"health"`
}

// EtcdClusterInfo represents overall cluster information
type EtcdClusterInfo struct {
	Members     []EtcdMemberV2 `json:"members"`
	LeaderID    string         `json:"leaderId"`
	LeaderName  string         `json:"leaderName"`
	ClusterID   string         `json:"clusterId"`
	RaftIndex   string         `json:"raftIndex"`
	DbSizeBytes int64          `json:"dbSizeBytes"`
	IsHealthy   bool           `json:"isHealthy"`
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

	// Step 3.5: etcd will not cleanly remove the member that is the current raft
	// leader — `member remove <leaderID>` silently no-ops and leaves the member
	// orphaned (bug #101). If the target is the leader, transfer leadership to a
	// healthy survivor first; only then does the removal commit. Determining the
	// leader is best-effort: if it can't be read we fall through to the removal,
	// which is correct for the common non-leader case (health already passed).
	if leaderID, err := getEtcdLeader(); err != nil {
		roslog.W("Could not determine etcd leader before removal; proceeding", err)
	} else if leaderID == memberID {
		roslog.I("Target is the current etcd leader; transferring leadership before removal", "memberId", memberID)
		if err := moveLeadershipAwayFrom(memberID); err != nil {
			return fmt.Errorf("refusing to remove etcd leader %s: %v", memberID, err)
		}
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

// buildMoveLeaderArgs assembles the etcdctl argv that transfers raft leadership
// to transfereeID. leaderEndpoint MUST be (or include) the CURRENT leader's own
// client endpoint: etcdctl move-leader locates the leader among the given
// --endpoints and errors ("no leader endpoint given") if none of them is the
// leader. Kept pure so the argv assembly is unit-testable without a live etcd.
func buildMoveLeaderArgs(leaderEndpoint, transfereeID string) []string {
	return []string{
		"--endpoints=" + leaderEndpoint,
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/peer.crt",
		"--key=/etc/kubernetes/pki/etcd/peer.key",
		"move-leader", transfereeID,
	}
}

// moveLeadershipAwayFrom transfers etcd raft leadership off targetID to a
// healthy, started, voting (non-learner) survivor and waits for it to settle.
// Required because etcd refuses to cleanly remove the current raft leader:
// removing the leader silently no-ops and orphans the member (bug #101).
//
// It returns an error WITHOUT removing anything when there is no eligible
// transferee or leadership does not move within the timeout, so the caller
// aborts rather than orphaning the member (Nodeward then retries/aborts).
func moveLeadershipAwayFrom(targetID string) error {
	members, err := listEtcdMembersDirectExtended()
	if err != nil {
		return fmt.Errorf("failed to list members for leader transfer: %v", err)
	}

	// Pick the first healthy, started, non-learner survivor as transferee, and
	// capture the target's own client URL (move-leader must be directed at the
	// current leader's endpoint) plus all client URLs as a fallback.
	var transferee *EtcdMemberV2
	var targetClientURLs string
	var allClientURLs []string
	for i := range members {
		m := &members[i]
		if m.ClientURLs != "" {
			allClientURLs = append(allClientURLs, m.ClientURLs)
		}
		if m.ID == targetID {
			targetClientURLs = m.ClientURLs
			continue
		}
		if transferee == nil && m.Started && !m.IsLearner && getMemberHealth(m.ClientURLs) == "healthy" {
			transferee = m
		}
	}
	if transferee == nil {
		return fmt.Errorf("no healthy non-learner survivor available to receive etcd leadership from %s", targetID)
	}

	// move-leader must hit the current leader (the target). Prefer the target's
	// own client URL; fall back to all members so etcdctl can still locate it.
	leaderEndpoint := targetClientURLs
	if leaderEndpoint == "" {
		leaderEndpoint = strings.Join(allClientURLs, ",")
	}

	roslog.I("Transferring etcd leadership before removal", "from", targetID, "to", transferee.ID)
	output, err := exec.Command("etcdctl", buildMoveLeaderArgs(leaderEndpoint, transferee.ID)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("etcdctl move-leader to %s failed: %v, output: %s", transferee.ID, err, string(output))
	}

	// Wait for leadership to settle off the target (~10s).
	for i := 0; i < 10; i++ {
		time.Sleep(1 * time.Second)
		leaderID, err := getEtcdLeader()
		if err != nil {
			roslog.W("Failed to read etcd leader while waiting for transfer to settle", err)
			continue
		}
		if leaderID != targetID {
			roslog.I("etcd leadership moved off target", "newLeader", leaderID, "target", targetID)
			return nil
		}
	}
	return fmt.Errorf("etcd leadership did not move off %s within timeout after move-leader", targetID)
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
	clusterID, raftIndex, dbSize, err := getEtcdStatus()
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
		RaftIndex:   raftIndex,
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

// getEtcdStatus returns the real cluster ID, the local endpoint's raft index,
// and the DB size in bytes. It uses `-w fields` (not `-w table`) because the
// table form does not expose the cluster ID at all, and its "ID" column is the
// endpoint's member ID, not the cluster ID. The fields form yields each value
// on its own `"Key" : value` line, so the values are read by name rather than
// by fragile positional parsing of a rendered table.
func getEtcdStatus() (clusterID, raftIndex string, dbSize int64, err error) {
	cmd := exec.Command("etcdctl",
		"--endpoints=https://127.0.0.1:2379",
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/peer.crt",
		"--key=/etc/kubernetes/pki/etcd/peer.key",
		"endpoint", "status", "-w", "fields")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to get etcd status: %v", err)
	}

	fields := parseEtcdStatusFields(string(output))
	clusterID = fields["ClusterID"]
	raftIndex = fields["RaftIndex"]
	if raftIndex == "" {
		// Older etcdctl emits "Revision" for the same value.
		raftIndex = fields["Revision"]
	}
	if sizeStr := fields["DbSize"]; sizeStr != "" {
		if size, parseErr := strconv.ParseInt(sizeStr, 10, 64); parseErr == nil {
			dbSize = size // DbSize is already in bytes
		}
	}

	if clusterID == "" && raftIndex == "" && dbSize == 0 {
		return "", "", 0, fmt.Errorf("status not found in output")
	}

	return clusterID, raftIndex, dbSize, nil
}

// parseEtcdStatusFields parses `etcdctl endpoint status -w fields` output, whose
// lines look like `"ClusterID" : 17237436991929494000`. Only the first endpoint
// block is needed (we query a single local endpoint), and later duplicate keys
// simply overwrite earlier ones.
func parseEtcdStatusFields(output string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.Trim(strings.TrimSpace(line[:idx]), `"`)
		val := strings.Trim(strings.TrimSpace(line[idx+1:]), `"`)
		if key != "" {
			fields[key] = val
		}
	}
	return fields
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
