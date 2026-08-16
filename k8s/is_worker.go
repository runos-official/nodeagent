package k8s

// IsWorker reports whether the current node can run regular workloads, by
// examining its node taints. When the read fails it carries the last known role
// rather than reporting false. See NodeRoleSnapshot.
func IsWorker() bool {
	return NodeRoleSnapshot().IsWorker
}

// GetStatus returns the current node's Kubernetes readiness status string.
// When the read fails it carries the last known status, or StatusUnknown if the
// agent has never read the node object. See NodeRoleSnapshot.
func GetStatus() string {
	return NodeRoleSnapshot().Status
}
