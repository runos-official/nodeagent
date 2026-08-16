package k8s

// IsCP reports whether the current node is a Kubernetes control plane node.
//
// The kubeadm static pod manifest is checked first, because it is a LOCAL fact
// that no API read failure can take away. The node labels are consulted only
// when the manifest is absent, which is the worker case. See NodeRoleSnapshot
// for the full reading and for the R6 incident this ordering exists for.
func IsCP() bool {
	return NodeRoleSnapshot().IsCp
}
