package preflight

import (
	"strings"
	"testing"
	"time"
)

// FCR 147, item 1. At install every nested guest printed
//
//	⚠ WARNING [etcd-fsync]: the disk backing /var/lib/etcd is slow
//	  (measured fsync p99 ~12 ms; etcd needs < 10 ms)
//
// RunOS warned and installed. The cluster ran on one control plane and on two control planes.
// The cluster collapsed when a third control plane joined, and it stayed collapsed.
// All three guests sat on ONE backing device: LINSTOR replicated-2 on one host's SAS spindles.
// etcd_disk_backend_commit_duration averaged 12.7 ms there, and the kube-apiserver missed its
// handler deadline on almost every request.
//
// The warning reported a measurement and no consequence. An operator cannot act on "12 ms".
// The operator does not learn that the disk caps how many control planes the cluster can carry,
// and forty minutes later the kube-apiserver crashloops with nothing pointing back at the disk.
//
// The warning fires for any p99 above 10 ms with no upper bound, and preflight does not know
// whether this node will be a control plane or a worker. So the tests below check what the text
// must NOT say as well as what it must say.

func TestSlowFsyncWarningKeepsTheMeasuredNumbers(t *testing.T) {
	got := resSlowFsyncWarning(12700*time.Microsecond, "/var").Error()
	if !strings.Contains(got, "~12.7 ms") {
		t.Errorf("the measured p99 must stay in the warning, got %q", got)
	}
	if !strings.Contains(got, "10 ms") {
		t.Errorf("etcd's 10 ms target must stay in the warning, got %q", got)
	}
	if !strings.Contains(got, "/var/lib/etcd") {
		t.Errorf("the warning must name the etcd data directory, got %q", got)
	}
	if !strings.Contains(got, "at /var") {
		t.Errorf("the warning must name the directory it actually benchmarked, got %q", got)
	}
	if !strings.Contains(got, "12.7 ms average") {
		t.Errorf("the measured field number must stay in the warning, got %q", got)
	}
}

// A p99 just over the threshold must not print a number that reads as compliant.
// %.0f rendered 10.4 ms as "~10 ms" next to a body saying the disk is unfit.
func TestSlowFsyncWarningDoesNotRoundIntoCompliance(t *testing.T) {
	got := resSlowFsyncWarning(10400*time.Microsecond, "/var").Error()
	if strings.Contains(got, "~10 ms;") {
		t.Errorf("a 10.4 ms reading must not print as ~10 ms, got %q", got)
	}
	if !strings.Contains(got, "~10.4 ms") {
		t.Errorf("the warning must print one decimal place, got %q", got)
	}
}

func TestSlowFsyncWarningStatesTheControlPlaneConsequence(t *testing.T) {
	got := strings.ToLower(resSlowFsyncWarning(12*time.Millisecond, "/var").Error())

	// THE POINT OF THIS FIX: the operator must read the consequence, not only the number.
	if !strings.Contains(got, "control plane") {
		t.Fatalf("the warning must say what a slow disk costs a control plane, got %q", got)
	}
	// The old text said only "etcd will be unstable". Keep that, it is true at every p99
	// this check fires at, and add the consequence the FCR asked for.
	if !strings.Contains(got, "unstable") {
		t.Errorf("the warning must keep the instability finding, got %q", got)
	}
	if !strings.Contains(got, "do not make this node a control plane") {
		t.Errorf("the warning must give the operator a node-scoped action, got %q", got)
	}
	if !strings.Contains(got, "crashloop") {
		t.Errorf("the warning must name the symptom the operator will actually see, got %q", got)
	}
	if !strings.Contains(got, "names the storage") {
		t.Errorf("the warning must say the later failure does not point back at the storage, got %q", got)
	}
}

// The check fires for any p99 above 10 ms and has no upper bound, and preflight measures one
// node, so the text may promise NOTHING in either direction. It may not say a topology runs
// normally: an operator on a failing HDD reads the same words as an operator at 10.1 ms. It may
// not say a topology is impossible either: FCR 147 ran two control planes on the storage that
// later collapsed, so "cannot carry more than one control plane" is disproved by the very case
// the warning cites.
func TestFsyncWarningPromisesNoOutcomeInEitherDirection(t *testing.T) {
	reassurance := []string{"run normally", "runs normally", "normally on a disk"}
	impossibility := []string{
		"cannot carry more than one",
		"cannot carry",
		"can only carry one",
		"will not run",
		"keep this cluster to a single control plane",
	}

	texts := map[string]string{
		"network-filesystem branch": resNetworkFsWarning(resMountInfo{mountPoint: "/var/lib", fsType: "nfs4"}).Error(),
	}
	for _, p99 := range []time.Duration{
		10100 * time.Microsecond,
		12700 * time.Microsecond,
		120 * time.Millisecond,
		500 * time.Millisecond,
		5 * time.Second,
	} {
		texts["p99="+p99.String()] = resSlowFsyncWarning(p99, "/var").Error()
	}

	for where, text := range texts {
		got := strings.ToLower(text)
		for _, banned := range reassurance {
			if strings.Contains(got, banned) {
				t.Errorf("%s claims something runs normally (%q): %q", where, banned, got)
			}
		}
		// A node-local reading may not prescribe, or forbid, a cluster topology it never
		// measured. Preflight cannot even tell a worker from a control plane.
		for _, banned := range impossibility {
			if strings.Contains(got, banned) {
				t.Errorf("%s asserts a topology is impossible (%q): %q", where, banned, got)
			}
		}
		// What replaces both: the measured case, and the risk named as unmeasured.
		if !strings.Contains(got, "nobody has measured") {
			t.Errorf("%s must name the added control plane as an unmeasured risk: %q", where, got)
		}
	}
}

// Raft commits on floor(n/2)+1 (conductor/src/etcd/guardrails.ts), so the fsync count on a
// write's critical path goes 1 -> 2 -> 2, and members fsync in parallel on their own devices.
// The warning must therefore not sell added control planes as a latency multiplier.
func TestSlowFsyncWarningStatesFsyncWorkNotAMultipliedLatency(t *testing.T) {
	got := strings.ToLower(resSlowFsyncWarning(12700*time.Microsecond, "/var").Error())
	if !strings.Contains(got, "quorum") {
		t.Errorf("the warning must say that every etcd write waits on a quorum fsync, got %q", got)
	}
	if !strings.Contains(got, "one more member fsync every write") {
		t.Errorf("the warning must state the added fsync work per control plane, got %q", got)
	}
	if !strings.Contains(got, "share one backing device") {
		t.Errorf("the warning must name the shared-device condition that turns work into latency, got %q", got)
	}
	for _, banned := range []string{"three times the fsync cost", "triples", "three times"} {
		if strings.Contains(got, banned) {
			t.Errorf("the warning multiplies fsync latency by member count (%q), which raft does not do: %q", banned, got)
		}
	}
}

// Both branches of checkEtcdDiskFsyncLatency describe one fault: a data disk that cannot meet
// etcd's fsync target. A network filesystem is the worse of the two, so it must not carry the
// milder message. The branch fires only on a real nfs/cifs/gluster/ceph mount under
// /var/lib/etcd, which the test runner does not have, so the test feeds a synthetic mount table
// through the real resMountFor selection and into the real branch.
func TestNetworkFilesystemBranchCarriesTheSameConsequence(t *testing.T) {
	for _, fsType := range []string{"nfs", "nfs4", "cifs", "smb", "smbfs", "fuse.glusterfs", "ceph"} {
		mounts := []resMountInfo{
			{source: "/dev/sda1", mountPoint: "/", fsType: "ext4"},
			{source: "storage:/export/etcd", mountPoint: "/var/lib", fsType: fsType},
		}
		m, ok := resMountFor("/var/lib/etcd", mounts)
		if !ok || m.fsType != fsType {
			t.Fatalf("%s: resMountFor picked %+v, ok=%v", fsType, m, ok)
		}
		err := resNetworkFsWarning(m)
		if err == nil {
			t.Errorf("%s must be flagged as a network filesystem", fsType)
			continue
		}
		got := err.Error()
		if !strings.Contains(got, fsType) || !strings.Contains(got, "/var/lib") {
			t.Errorf("%s: the finding must name the filesystem and its mount point, got %q", fsType, got)
		}
		if !strings.Contains(got, resFsyncConsequence) {
			t.Errorf("%s: the network-filesystem branch must end with the shared consequence, got %q", fsType, got)
		}
	}

	// A local filesystem must fall through to the benchmark, not to this branch.
	for _, fsType := range []string{"ext4", "xfs", "btrfs", "overlay", "zfs"} {
		if err := resNetworkFsWarning(resMountInfo{mountPoint: "/var", fsType: fsType}); err != nil {
			t.Errorf("%s is a local filesystem and must not take the network branch, got %q", fsType, err)
		}
	}

	// The consequence itself, which both branches share.
	if !strings.Contains(resFsyncConsequence, "Do not make this node a control plane") {
		t.Errorf("the shared consequence must give a node-scoped action, got %q", resFsyncConsequence)
	}
	if !strings.Contains(resFsyncConsequence, "quorum") {
		t.Errorf("the shared consequence must state the quorum fsync mechanism, got %q", resFsyncConsequence)
	}
	if !strings.Contains(resFsyncConsequence, "12.7 ms average") {
		t.Errorf("the shared consequence must keep the measured field number, got %q", resFsyncConsequence)
	}
	// The slow-disk branch reaches the operator through the same tail.
	if !strings.Contains(resSlowFsyncWarning(12*time.Millisecond, "/var").Error(), resFsyncConsequence) {
		t.Error("the slow-disk branch must end with the shared consequence")
	}
}

// Item 1 rewrote the wording only. It did not change the severity, and this test records that
// rather than fixing the severity in place. FCR 147 fix item 2 is chartered to re-check fsync at
// control-plane join and to decide whether that point refuses; if item 2 changes the severity
// here, update or delete this test deliberately.
func TestEtcdFsyncSeverityUnchangedByItem1(t *testing.T) {
	for _, c := range preflightChecks() {
		if c.name != "etcd-fsync" {
			continue
		}
		if c.sev != sevWarn {
			t.Fatalf("item 1 did not change the etcd-fsync severity, got %v", c.sev)
		}
		if c.fatal {
			t.Fatal("item 1 did not make etcd-fsync a fatal prerequisite")
		}
		return
	}
	t.Fatal("no etcd-fsync check is registered")
}
