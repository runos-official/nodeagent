package k8s

import (
	"fmt"
)

// buildRemoveEtcdMemberCmd assembles the shell command that removes the etcd
// member memberID by exec-ing etcdctl inside the given etcd pod. Kept as a pure
// function so the argument assembly can be unit-tested without invoking etcdctl.
//
// Note: this kubectl-exec form is retained only for the pinned unit test. The
// live removal paths (the CLI `runos etcd remove` and the REMOVE_ETCD_MEMBER
// instruction handler) use the quorum-guarded RemoveEtcdMemberDirect /
// RemoveEtcdMemberViaEtcdCtlByIP in remove_etcd_member.go, which verify cluster
// health and quorum before removing and then poll for propagation instead of
// blind-sleeping.
func buildRemoveEtcdMemberCmd(etcdPodName, memberID string) string {
	return fmt.Sprintf(`kubectl -n kube-system exec -it %s -- etcdctl `+
		`--endpoints=localhost:2379 `+
		`--cacert=/etc/kubernetes/pki/etcd/ca.crt `+
		`--cert=/etc/kubernetes/pki/etcd/server.crt `+
		`--key=/etc/kubernetes/pki/etcd/server.key `+
		`member remove %s`, etcdPodName, memberID)
}
