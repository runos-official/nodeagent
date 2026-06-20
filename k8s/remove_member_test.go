package k8s

import (
	"strings"
	"testing"
)

// TestBuildRemoveEtcdMemberCmd asserts the etcd member-removal command is
// assembled with the correct pod name, member ID, namespace, etcdctl endpoint
// and TLS material. This is the high-risk argument assembly: a wrong endpoint or
// cert path silently fails member removal, so pin the exact shape.
func TestBuildRemoveEtcdMemberCmd(t *testing.T) {
	cmd := buildRemoveEtcdMemberCmd("etcd-node-1", "8e9e05c52164694d")

	mustContain := []string{
		"kubectl -n kube-system exec -it etcd-node-1 -- etcdctl",
		"--endpoints=localhost:2379",
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/server.crt",
		"--key=/etc/kubernetes/pki/etcd/server.key",
		"member remove 8e9e05c52164694d",
	}
	for _, sub := range mustContain {
		if !strings.Contains(cmd, sub) {
			t.Errorf("command missing %q\nfull command: %s", sub, cmd)
		}
	}

	// The member ID must be the last token (the remove target), and the pod name
	// must appear before the `--` separator that ends kubectl's own args.
	if !strings.HasSuffix(cmd, "member remove 8e9e05c52164694d") {
		t.Errorf("expected command to end with the remove target, got: %s", cmd)
	}
	dashIdx := strings.Index(cmd, " -- ")
	podIdx := strings.Index(cmd, "etcd-node-1")
	if dashIdx < 0 || podIdx < 0 || podIdx > dashIdx {
		t.Errorf("expected pod name before the `--` separator, got: %s", cmd)
	}
}

// TestBuildRemoveEtcdMemberCmd_DistinctInputs guards against the pod name and
// member ID being swapped or hardcoded.
func TestBuildRemoveEtcdMemberCmd_DistinctInputs(t *testing.T) {
	a := buildRemoveEtcdMemberCmd("etcd-podA", "111")
	b := buildRemoveEtcdMemberCmd("etcd-podB", "222")
	if a == b {
		t.Fatal("expected distinct commands for distinct inputs")
	}
	if !strings.Contains(a, "etcd-podA") || !strings.Contains(a, "member remove 111") {
		t.Errorf("command A did not reflect its inputs: %s", a)
	}
	if !strings.Contains(b, "etcd-podB") || !strings.Contains(b, "member remove 222") {
		t.Errorf("command B did not reflect its inputs: %s", b)
	}
}
