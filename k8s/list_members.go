package k8s

import (
	"fmt"
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/roslog"
	"os/exec"
	"strings"
)

// EtcdMember represents an etcd cluster member
type EtcdMember struct {
	ID         string
	Name       string
	PeerURLs   []string
	ClientURLs []string
}

// ListEtcdMembers retrieves all etcd cluster members
func ListEtcdMembers() ([]EtcdMember, error) {
	// Get current hostname for constructing etcd pod name
	hostname, err := commons.GetHostname()
	if err != nil {
		roslog.E("Error getting hostname", err)
		return nil, err
	}

	// Construct etcd pod name using hostname
	etcdPodName := fmt.Sprintf("etcd-%s", hostname)

	// Execute kubectl command to get etcd member list
	cmd := fmt.Sprintf(`kubectl -n kube-system exec -it %s -- etcdctl `+
		`--endpoints=localhost:2379 `+
		`--cacert=/etc/kubernetes/pki/etcd/ca.crt `+
		`--cert=/etc/kubernetes/pki/etcd/server.crt `+
		`--key=/etc/kubernetes/pki/etcd/server.key `+
		`member list`, etcdPodName)

	output, err := exec.Command("bash", "-c", cmd).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list etcd members: %v", err)
	}

	// Parse the output to extract member details
	members := []EtcdMember{}
	lines := strings.Split(string(output), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Split(line, ", ")
		if len(parts) < 3 {
			continue
		}

		// Format: ID, name, peerURLs, clientURLs
		id := parts[0]
		name := strings.TrimPrefix(parts[1], "name=")
		peerURLs := strings.Split(strings.TrimPrefix(parts[2], "peerURLs="), ",")

		var clientURLs []string
		if len(parts) > 3 {
			clientURLs = strings.Split(strings.TrimPrefix(parts[3], "clientURLs="), ",")
		}

		members = append(members, EtcdMember{
			ID:         id,
			Name:       name,
			PeerURLs:   peerURLs,
			ClientURLs: clientURLs,
		})
	}

	return members, nil
}

// FindEtcdMemberByIP locates an etcd member by the given IP address
func FindEtcdMemberByIP(nodeIP string) (*EtcdMember, error) {
	members, err := ListEtcdMembers()
	if err != nil {
		return nil, err
	}

	for _, member := range members {
		// Check in peerURLs
		for _, url := range member.PeerURLs {
			if strings.Contains(url, nodeIP) {
				return &member, nil
			}
		}

		// Also check in clientURLs
		for _, url := range member.ClientURLs {
			if strings.Contains(url, nodeIP) {
				return &member, nil
			}
		}
	}

	return nil, fmt.Errorf("could not find etcd member with IP %s", nodeIP)
}
