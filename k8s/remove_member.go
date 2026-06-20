package k8s

import (
	"fmt"
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/roslog"
	"os/exec"
	"time"
)

// buildRemoveEtcdMemberCmd assembles the shell command that removes the etcd
// member memberID by exec-ing etcdctl inside the given etcd pod. Kept as a pure
// function so the argument assembly can be unit-tested without invoking etcdctl.
func buildRemoveEtcdMemberCmd(etcdPodName, memberID string) string {
	return fmt.Sprintf(`kubectl -n kube-system exec -it %s -- etcdctl `+
		`--endpoints=localhost:2379 `+
		`--cacert=/etc/kubernetes/pki/etcd/ca.crt `+
		`--cert=/etc/kubernetes/pki/etcd/server.crt `+
		`--key=/etc/kubernetes/pki/etcd/server.key `+
		`member remove %s`, etcdPodName, memberID)
}

// RemoveEtcdMember removes an etcd member by its ID
func RemoveEtcdMember(etcdPodName, memberID string) error {
	roslog.I("RemoveEtcdMember", memberID)
	// Execute kubectl command to remove etcd member
	cmd := buildRemoveEtcdMemberCmd(etcdPodName, memberID)

	output, err := exec.Command("bash", "-c", cmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to remove etcd member: %v, output: %s", err, string(output))
	}

	// Sleep for a short duration to allow the removal to propagate
	time.Sleep(20 * time.Second)

	return nil
}

// RemoveEtcdMemberByID removes the etcd member with the given ID, locating the
// local etcd pod via the node hostname.
func RemoveEtcdMemberByID(memberID string) error {
	hostname, err := commons.GetHostname()
	if err != nil {
		roslog.E("Error getting hostname", err)
		return err
	}

	// Construct etcd pod name using hostname
	etcdPodName := fmt.Sprintf("etcd-%s", hostname)

	// Then remove it
	return RemoveEtcdMember(etcdPodName, memberID)
}

// RemoveEtcdMemberByIP finds and removes an etcd member by IP address
func RemoveEtcdMemberByIP(nodeIP string) error {
	// Get current hostname for constructing etcd pod name
	hostname, err := commons.GetHostname()
	if err != nil {
		roslog.E("Error getting hostname", err)
		return err
	}

	// Construct etcd pod name using hostname
	etcdPodName := fmt.Sprintf("etcd-%s", hostname)

	// First find the member with the given IP
	member, err := FindEtcdMemberByIP(nodeIP)
	if err != nil {
		return err
	}

	// Then remove it
	return RemoveEtcdMember(etcdPodName, member.ID)
}
