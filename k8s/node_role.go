package k8s

import (
	"encoding/json"
	"os"
	"strings"
	"sync"

	"github.com/runos-official/nodeagent/roslog"
)

// StatusUnknown is the status reported when the agent has never managed to read
// the node object and therefore does not know the node's readiness. It is NOT
// not_ready: a failed read is not evidence that the node is unhealthy.
const StatusUnknown = "unknown"

const (
	// kubeAPIServerManifestPath is the kubeadm static pod manifest. Its presence
	// is the local, proxy-independent fact that this node is a control plane.
	kubeAPIServerManifestPath = "/etc/kubernetes/manifests/kube-apiserver.yaml"
	// adminKubeconfigPath holds the credentials a control plane uses to talk to
	// its own API server.
	adminKubeconfigPath = "/etc/kubernetes/admin.conf"
	// localAPIServerURL is this node's own kube-apiserver. The kubeadm config
	// RunOS writes lists 127.0.0.1 in apiServer.certSANs, so the serving
	// certificate covers this address (nodeward uc/prep/25_first_node.go).
	localAPIServerURL = "https://127.0.0.1:6443"
)

// RoleSnapshot is one reading of the local node's role and readiness.
type RoleSnapshot struct {
	IsCp     bool
	IsWorker bool
	Status   string
	// RolesKnown is true when the roles come from a fresh, successful read of the
	// node object. It is false when they are carried from the last known reading
	// or derived from a local fact alone. Nodeward refuses to demote a node on a
	// heartbeat whose roles are not known.
	RolesKnown bool
}

// Seams. Tests replace these to drive the role logic without a live cluster.
var (
	runKubectl = func(args ...string) ([]byte, error) { return kubectlWithTimeout(args...) }
	statFile   = func(path string) bool { _, err := os.Stat(path); return err == nil }
	hostnameFn = os.Hostname
)

// lastKnown holds the most recent SUCCESSFUL reading, so a transient read failure
// carries the previous role and status instead of inventing a demotion.
//
// R6 (2026-08-16, cluster ede): one of three control planes was hard powered off.
// The heartbeat read the node object through the agent's own kube proxy, the proxy
// lost its targets, the read failed, and IsCP / IsWorker / GetStatus returned
// false / false / not_ready. Nodeward wrote those values over the truth, so its
// control-plane list emptied, every agent's proxy lost its backends, and every
// remaining node reported false in turn. Kubernetes was healthy with etcd quorum
// throughout. One dead control plane took RunOS's whole view of the cluster down.
var (
	lastKnownMu sync.RWMutex
	lastKnown   RoleSnapshot
	lastKnownOk bool
)

// resetRoleCache clears the last known reading. Tests only.
func resetRoleCache() {
	lastKnownMu.Lock()
	defer lastKnownMu.Unlock()
	lastKnown = RoleSnapshot{}
	lastKnownOk = false
}

func storeLastKnown(snap RoleSnapshot) {
	lastKnownMu.Lock()
	defer lastKnownMu.Unlock()
	lastKnown = snap
	lastKnownOk = true
}

func loadLastKnown() (RoleSnapshot, bool) {
	lastKnownMu.RLock()
	defer lastKnownMu.RUnlock()
	return lastKnown, lastKnownOk
}

// nodeObject is the slice of a Kubernetes Node this agent reads.
type nodeObject struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Unschedulable bool `json:"unschedulable"`
		Taints        []struct {
			Key    string `json:"key"`
			Effect string `json:"effect"`
		} `json:"taints"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

// NodeRoleSnapshot reports the local node's role and readiness in one reading.
//
// A control plane asks its OWN API server first, because the proxied path shares
// its fate with the control-plane list this reading feeds. Only when that fails
// does it fall back to the proxy. When every read fails, the snapshot carries the
// last known values and sets RolesKnown=false.
func NodeRoleSnapshot() RoleSnapshot {
	manifestCp := statFile(kubeAPIServerManifestPath)

	hostname, err := hostnameFn()
	if err != nil {
		roslog.E("Error getting hostname", err)
		return carriedSnapshot(manifestCp)
	}

	output, err := readNodeObject(hostname, manifestCp)
	if err != nil {
		roslog.W("Could not read the local node object, carrying the last known role and status", nil,
			"error", err.Error(), "isCpFromManifest", manifestCp)
		return carriedSnapshot(manifestCp)
	}

	var obj nodeObject
	if err := json.Unmarshal(output, &obj); err != nil {
		roslog.E("Error parsing kubectl output", err)
		return carriedSnapshot(manifestCp)
	}

	snap := RoleSnapshot{
		IsCp:       manifestCp || hasControlPlaneLabel(obj),
		IsWorker:   !hasControlPlaneNoScheduleTaint(obj),
		Status:     statusOf(obj),
		RolesKnown: true,
	}
	storeLastKnown(snap)

	return snap
}

// readNodeObject fetches this node's object as JSON. On a control plane it tries
// the node's own API server before the proxied path.
func readNodeObject(hostname string, manifestCp bool) ([]byte, error) {
	if manifestCp && statFile(adminKubeconfigPath) {
		output, err := runKubectl("--kubeconfig", adminKubeconfigPath, "--server", localAPIServerURL,
			"get", "node", hostname, "-o", "json")
		if err == nil {
			return output, nil
		}
		roslog.W("Local API server read failed, falling back to the proxied path", nil, "error", err.Error())
	}

	return runKubectl("get", "node", hostname, "-o", "json")
}

// carriedSnapshot builds the snapshot used when no fresh read succeeded. It never
// invents a demotion: the local static pod manifest still settles the control
// plane role, and everything else is carried from the last known reading.
func carriedSnapshot(manifestCp bool) RoleSnapshot {
	snap := RoleSnapshot{Status: StatusUnknown}

	if last, ok := loadLastKnown(); ok {
		snap.IsCp = last.IsCp
		snap.IsWorker = last.IsWorker
		snap.Status = last.Status
	}

	if manifestCp {
		snap.IsCp = true
	}

	return snap
}

func hasControlPlaneLabel(obj nodeObject) bool {
	for label := range obj.Metadata.Labels {
		if strings.HasPrefix(label, "node-role.kubernetes.io/control-plane") ||
			strings.HasPrefix(label, "node-role.kubernetes.io/master") {
			return true
		}
	}
	return false
}

func hasControlPlaneNoScheduleTaint(obj nodeObject) bool {
	for _, taint := range obj.Spec.Taints {
		if (taint.Key == "node-role.kubernetes.io/control-plane" ||
			taint.Key == "node-role.kubernetes.io/master") &&
			taint.Effect == "NoSchedule" {
			return true
		}
	}
	return false
}

func statusOf(obj nodeObject) string {
	if obj.Spec.Unschedulable {
		return "cordoned"
	}

	for _, condition := range obj.Status.Conditions {
		if condition.Type == "Ready" {
			if condition.Status == "True" {
				return "ready"
			}
			return "not_ready"
		}
	}

	return "not_ready"
}
