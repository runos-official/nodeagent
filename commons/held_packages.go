package commons

import "strings"

// RunosPackage is one package the RunOS node install puts on a node and that
// RunOS owns: it holds the package, moves it, or removes it on a node delete.
//
// ONE DECLARATION, because four hand-maintained lists of the same set had
// drifted (FCR 933). The hold loop, the unhold loop, the Kubernetes upgrade's
// move list and the uninstall purge list are all derived from this table, so a
// name can no longer be in one and missing from another. The defect that proves
// why: the purge list omitted cri-tools while the unhold loop released it, so a
// node delete released the hold and left cri-tools installed.
type RunosPackage struct {
	// Name is the Debian package name.
	Name string

	// Held says RunOS holds this package, so a RunOS operation that moves or
	// removes it must release the hold first.
	//
	// It does NOT mean the hold is already set on every node. Measured on
	// 2026-11-08 on two Ubuntu 24.04 nodes, one control plane and one worker: a
	// node install holds the Kubernetes trio only, a node that has taken a RunOS
	// Kubernetes upgrade also holds the three UpgradeMoved packages, and
	// haproxy, dnsmasq and wireguard-tools are held on neither, because the
	// install-hold half that sets them is reverted. The unhold and purge loops
	// must still cover all of them: a loop sized to what one node happens to
	// hold would leave another node's holds in place.
	Held bool

	// Kubernetes says this package moves only through an explicit Kubernetes
	// upgrade, never through a maintenance run.
	Kubernetes bool

	// UpgradeMoved says the Kubernetes node upgrade may move this package, so
	// that upgrade releases its hold and sets it again afterwards.
	UpgradeMoved bool
}

// RunosPackages is the authoritative set. Every list below derives from it.
//
// The set is the ten names the node agent already treats as RunOS-owned. The
// RunOS install puts 27 packages on a node, measured on 2026-11-08 on an Ubuntu
// 24.04 control plane and worker, and the other 17 are deliberately NOT here:
// apt-transport-https, ca-certificates, curl, gpg, etcd-client, jq, htop,
// net-tools, tcpdump, mtr-tiny, dnsutils, iotop, nethogs, ncdu, strace, lsof,
// vim-tiny, bpftrace and kubectx. Purging ca-certificates, curl or gpg would
// break TLS and apt on the node, so widening the purge set to the whole install
// list would be a worse defect than the one this table fixes. Whether
// etcd-client and the operator tools should be RunOS-owned is a separate
// decision.
var RunosPackages = []RunosPackage{
	// The Kubernetes trio. The node install holds these, and only an explicit
	// Kubernetes upgrade moves them.
	{Name: "kubelet", Held: true, Kubernetes: true},
	{Name: "kubeadm", Held: true, Kubernetes: true},
	{Name: "kubectl", Held: true, Kubernetes: true},

	// The runtime packages a Kubernetes node upgrade may move: containerd goes
	// to the version the matrix names, and kubeadm and kubelet pull in a newer
	// cri-tools and kubernetes-cni when their minimum changes.
	{Name: "containerd", Held: true, UpgradeMoved: true},
	{Name: "cri-tools", Held: true, UpgradeMoved: true},
	{Name: "kubernetes-cni", Held: true, UpgradeMoved: true},

	// The node service packages. A Kubernetes upgrade never moves these; a
	// controlled maintenance run takes their security revisions.
	{Name: "haproxy", Held: true},
	{Name: "dnsmasq", Held: true},
	{Name: "wireguard-tools", Held: true},

	// Purge-only. The current install puts wireguard-tools on a node and NOT
	// wireguard, measured on 2026-11-08 on an Ubuntu 24.04 control plane and
	// worker, so RunOS never holds wireguard. The name stays in the purge set
	// because an older installer may have placed it on an existing node, and
	// dropping the name would leave that package behind on exactly the nodes
	// that have it.
	{Name: "wireguard"},
}

// The derived lists. Every one is built from RunosPackages at package init, so
// adding or removing a row updates all of them together.
var (
	// KubernetesHeldPackages are the packages that move only through an explicit
	// Kubernetes upgrade.
	KubernetesHeldPackages = namesWhere(func(p RunosPackage) bool { return p.Kubernetes })

	// UpgradeMovedPackages are the held packages the Kubernetes node upgrade can
	// move, so it releases their holds and sets them again afterwards.
	UpgradeMovedPackages = namesWhere(func(p RunosPackage) bool { return p.UpgradeMoved })

	// RuntimeHeldPackages are the held runtime and node service packages, which
	// is every held package besides the Kubernetes trio.
	RuntimeHeldPackages = namesWhere(func(p RunosPackage) bool { return p.Held && !p.Kubernetes })

	// HeldPackages is every package RunOS holds. A RunOS operation that moves or
	// removes one of these must release its hold first, so the uninstall releases
	// all of them and the purge set covers all of them.
	HeldPackages = namesWhere(func(p RunosPackage) bool { return p.Held })

	// PurgePackages is every declared package, held or not. It is a superset of
	// HeldPackages BY CONSTRUCTION, which is the property the uninstall needs: a
	// name the unhold loop releases and the purge does not name is left
	// installed on the node.
	PurgePackages = namesWhere(func(RunosPackage) bool { return true })
)

func namesWhere(keep func(RunosPackage) bool) []string {
	out := make([]string, 0, len(RunosPackages))
	for _, p := range RunosPackages {
		if keep(p) {
			out = append(out, p.Name)
		}
	}
	return out
}

// AptMarkInstalledCommand returns a shell command that runs
// `apt-mark <action> <pkg>` for each package that is installed, and skips the
// ones that are not (apt-mark hold on an absent package would leave a stale
// selection; unhold on an unknown name fails). action is "hold" or "unhold". The
// command exits non-zero when apt-mark fails for an installed package. The
// package names are constants of this package, never request values.
func AptMarkInstalledCommand(action string, pkgs []string) string {
	return "for p in " + strings.Join(pkgs, " ") + "; do " +
		`if [ "$(dpkg-query -W -f='${db:Status-Status}' "$p" 2>/dev/null)" = installed ]; then ` +
		"apt-mark " + action + ` "$p" || exit 1; fi; done`
}
