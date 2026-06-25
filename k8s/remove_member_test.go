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

// TestBuildMoveLeaderArgs asserts the leadership-transfer argv is assembled with
// the leader endpoint, TLS material and the transferee as the final token. This
// runs before removing an etcd member that is the current raft leader (bug #101);
// a wrong endpoint or a swapped transferee silently fails the transfer and the
// subsequent leader-removal would orphan the member, so pin the exact shape.
func TestBuildMoveLeaderArgs(t *testing.T) {
	args := buildMoveLeaderArgs("https://10.0.0.1:2379", "a8208c4dee07b533")
	joined := strings.Join(args, " ")

	mustContain := []string{
		"--endpoints=https://10.0.0.1:2379",
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/peer.crt",
		"--key=/etc/kubernetes/pki/etcd/peer.key",
		"move-leader",
		"a8208c4dee07b533",
	}
	for _, sub := range mustContain {
		if !strings.Contains(joined, sub) {
			t.Errorf("args missing %q\nfull args: %s", sub, joined)
		}
	}

	// The transferee ID must be the last arg (the move-leader target), and the
	// subcommand must immediately precede it.
	if got := args[len(args)-1]; got != "a8208c4dee07b533" {
		t.Errorf("expected transferee ID as the last arg, got %q (full: %s)", got, joined)
	}
	if got := args[len(args)-2]; got != "move-leader" {
		t.Errorf("expected \"move-leader\" immediately before the transferee, got %q", got)
	}

	// The endpoint must be passed via --endpoints (move-leader has to be directed
	// at the current leader's own endpoint), not as a positional arg.
	if !strings.HasPrefix(args[0], "--endpoints=") {
		t.Errorf("expected first arg to be the --endpoints flag, got %q", args[0])
	}
}

// TestBuildMoveLeaderArgs_DistinctInputs guards against the endpoint and
// transferee being swapped or hardcoded.
func TestBuildMoveLeaderArgs_DistinctInputs(t *testing.T) {
	a := strings.Join(buildMoveLeaderArgs("https://10.0.0.1:2379", "111"), " ")
	b := strings.Join(buildMoveLeaderArgs("https://10.0.0.2:2379", "222"), " ")
	if a == b {
		t.Fatal("expected distinct args for distinct inputs")
	}
	if !strings.Contains(a, "--endpoints=https://10.0.0.1:2379") || !strings.HasSuffix(a, "move-leader 111") {
		t.Errorf("args A did not reflect its inputs: %s", a)
	}
	if !strings.Contains(b, "--endpoints=https://10.0.0.2:2379") || !strings.HasSuffix(b, "move-leader 222") {
		t.Errorf("args B did not reflect its inputs: %s", b)
	}
}
