package k8s

import (
	"errors"
	"strings"
	"testing"
)

// R6 (2026-08-16, a test cluster). One control plane of three was hard powered off.
// Within two minutes RunOS marked the two SURVIVING control planes not_ready with
// isCp=false, because every read in the heartbeat path went through the agent's
// own kube proxy. The proxy had no live target, the reads failed, and IsCP /
// IsWorker / GetStatus each INVENTED false / false / not_ready. Nodeward wrote
// those values, the control-plane list it hands back emptied, every agent's proxy
// lost its targets, and the whole cluster's RunOS view collapsed while Kubernetes
// itself stayed healthy with etcd quorum.
//
// The rule these tests pin: a failed read is NOT evidence of a role change.

// fakeKubectl installs a scripted kubectl and records the argv of every call.
// It returns the recorded call list and restores the seams when the test ends.
func fakeKubectl(t *testing.T, respond func(args []string) ([]byte, error)) *[][]string {
	t.Helper()

	var calls [][]string

	origRun, origStat, origHost := runKubectl, statFile, hostnameFn
	t.Cleanup(func() {
		runKubectl, statFile, hostnameFn = origRun, origStat, origHost
		resetRoleCache()
	})

	runKubectl = func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return respond(args)
	}
	hostnameFn = func() (string, error) { return "n04", nil }
	statFile = func(string) bool { return false }

	resetRoleCache()

	return &calls
}

// readyWorkerJSON is a node object for a schedulable, Ready worker.
const readyWorkerJSON = `{
  "metadata": {"labels": {"kubernetes.io/hostname": "n04"}},
  "spec": {"taints": []},
  "status": {"conditions": [{"type": "Ready", "status": "True"}]}
}`

// readyControlPlaneJSON is a node object for a Ready, tainted control plane.
const readyControlPlaneJSON = `{
  "metadata": {"labels": {"node-role.kubernetes.io/control-plane": ""}},
  "spec": {"taints": [{"key": "node-role.kubernetes.io/control-plane", "effect": "NoSchedule"}]},
  "status": {"conditions": [{"type": "Ready", "status": "True"}]}
}`

// TestAFailedReadNeverDemotesAControlPlane is the R6 regression. The node still
// runs the kube-apiserver static pod, so it IS a control plane, whatever the
// proxied read says.
func TestAFailedReadNeverDemotesAControlPlane(t *testing.T) {
	fakeKubectl(t, func([]string) ([]byte, error) {
		return nil, errors.New("Unable to connect to the server: EOF")
	})
	// The kubeadm static pod manifest is the local fact that settles the role.
	statFile = func(path string) bool { return path == kubeAPIServerManifestPath }

	snap := NodeRoleSnapshot()

	if !snap.IsCp {
		t.Error("a node running the kube-apiserver static pod is a control plane, even when every read fails")
	}
	if snap.RolesKnown {
		t.Error("RolesKnown must be false when no fresh read succeeded, so nodeward refuses to write the roles")
	}
	if snap.Status == "not_ready" {
		t.Error("a failed read must not be reported as not_ready: it is not evidence the node is unhealthy")
	}
}

// TestAFailedReadCarriesTheLastKnownRoleAndStatus covers a worker, which has no
// static pod manifest to fall back on.
func TestAFailedReadCarriesTheLastKnownRoleAndStatus(t *testing.T) {
	fail := false
	fakeKubectl(t, func([]string) ([]byte, error) {
		if fail {
			return nil, errors.New("Unable to connect to the server: EOF")
		}
		return []byte(readyWorkerJSON), nil
	})

	first := NodeRoleSnapshot()
	if !first.RolesKnown || !first.IsWorker || first.Status != "ready" {
		t.Fatalf("seed read wrong: %+v", first)
	}

	fail = true
	carried := NodeRoleSnapshot()

	if !carried.IsWorker {
		t.Error("a failed read must carry the last known worker role, not invent isWorker=false")
	}
	if carried.Status != "ready" {
		t.Errorf("a failed read must carry the last known status, got %q", carried.Status)
	}
	if carried.RolesKnown {
		t.Error("carried roles must be marked RolesKnown=false")
	}
}

// TestAFailedReadWithNoHistoryReportsUnknown pins the cold-start case: the agent
// has never read the node object, so it must say so instead of guessing not_ready.
func TestAFailedReadWithNoHistoryReportsUnknown(t *testing.T) {
	fakeKubectl(t, func([]string) ([]byte, error) {
		return nil, errors.New("Unable to connect to the server: EOF")
	})

	snap := NodeRoleSnapshot()

	if snap.Status != StatusUnknown {
		t.Errorf("Status = %q, want %q: the agent has no basis to claim not_ready", snap.Status, StatusUnknown)
	}
	if snap.RolesKnown {
		t.Error("RolesKnown must be false with no successful read")
	}
}

// TestAControlPlaneAsksItsOwnApiServerFirst is the other half of R6: n04's own
// kube-apiserver was up and serving the whole time. Reading it directly keeps a
// surviving control plane reporting ready while the proxy has no targets.
func TestAControlPlaneAsksItsOwnApiServerFirst(t *testing.T) {
	calls := fakeKubectl(t, func(args []string) ([]byte, error) {
		if !argvHas(args, "--server", localAPIServerURL) {
			return nil, errors.New("Unable to connect to the server: EOF")
		}
		return []byte(readyControlPlaneJSON), nil
	})
	statFile = func(path string) bool {
		return path == kubeAPIServerManifestPath || path == adminKubeconfigPath
	}

	snap := NodeRoleSnapshot()

	if snap.Status != "ready" {
		t.Errorf("Status = %q, want ready: the node's own API server answered", snap.Status)
	}
	if !snap.IsCp || !snap.RolesKnown {
		t.Errorf("a direct read of the local API server is a fresh read: %+v", snap)
	}
	first := (*calls)[0]
	if !argvHas(first, "--server", localAPIServerURL) || !argvHas(first, "--kubeconfig", adminKubeconfigPath) {
		t.Errorf("first call must target the local API server with admin.conf, got %v", first)
	}
}

// TestAControlPlaneFallsBackToTheProxy keeps the old path alive for a control
// plane whose own API server is genuinely down.
func TestAControlPlaneFallsBackToTheProxy(t *testing.T) {
	calls := fakeKubectl(t, func(args []string) ([]byte, error) {
		if argvHas(args, "--server", localAPIServerURL) {
			return nil, errors.New("connection refused")
		}
		return []byte(readyControlPlaneJSON), nil
	})
	statFile = func(path string) bool {
		return path == kubeAPIServerManifestPath || path == adminKubeconfigPath
	}

	snap := NodeRoleSnapshot()

	if snap.Status != "ready" || !snap.RolesKnown {
		t.Errorf("the proxied read succeeded, so the snapshot is fresh: %+v", snap)
	}
	if len(*calls) < 2 {
		t.Fatalf("expected a fallback call after the local API server failed, got %d calls", len(*calls))
	}
}

// TestAFreshReadStaysAuthoritative guards against the fix over-reaching: a real
// demotion, seen by a successful read, must still be reported.
func TestAFreshReadStaysAuthoritative(t *testing.T) {
	body := readyControlPlaneJSON
	fakeKubectl(t, func([]string) ([]byte, error) { return []byte(body), nil })

	if snap := NodeRoleSnapshot(); !snap.IsCp || snap.IsWorker {
		t.Fatalf("seed read wrong: %+v", snap)
	}

	body = readyWorkerJSON
	snap := NodeRoleSnapshot()

	if snap.IsCp {
		t.Error("a successful read that shows no control-plane label or manifest must report isCp=false")
	}
	if !snap.IsWorker || !snap.RolesKnown || snap.Status != "ready" {
		t.Errorf("fresh read must be authoritative: %+v", snap)
	}
}

// TestACordonedNodeIsStillReported keeps the pre-existing cordoned signal.
func TestACordonedNodeIsStillReported(t *testing.T) {
	fakeKubectl(t, func([]string) ([]byte, error) {
		return []byte(`{"metadata":{"labels":{}},"spec":{"unschedulable":true,"taints":[]},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`), nil
	})

	if got := GetStatus(); got != "cordoned" {
		t.Errorf("GetStatus() = %q, want cordoned", got)
	}
}

// TestANotReadyNodeIsStillReported keeps a genuine NotReady reading intact.
func TestANotReadyNodeIsStillReported(t *testing.T) {
	fakeKubectl(t, func([]string) ([]byte, error) {
		return []byte(`{"metadata":{"labels":{}},"spec":{"taints":[]},"status":{"conditions":[{"type":"Ready","status":"False"}]}}`), nil
	})

	if got := GetStatus(); got != "not_ready" {
		t.Errorf("GetStatus() = %q, want not_ready", got)
	}
}

// argvHas reports whether flag is present with the given value, in either the
// "--flag value" or "--flag=value" form.
func argvHas(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
		if a == flag+"="+value {
			return true
		}
		if strings.HasPrefix(a, flag+"=") && strings.TrimPrefix(a, flag+"=") == value {
			return true
		}
	}
	return false
}
