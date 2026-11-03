package commons

import "strings"

// Packages RunOS installs at a chosen version and keeps there with apt-mark
// hold (FCR 933). Node install (nodeward) holds them after it installs them; a
// plain apt-get upgrade then leaves them alone, and only a RunOS operation that
// moves them (the Kubernetes upgrade, the uninstall) releases the hold first.
var (
	// KubernetesHeldPackages are the packages the node install has always held.
	KubernetesHeldPackages = []string{"kubelet", "kubeadm", "kubectl"}

	// UpgradeMovedPackages are the held packages the Kubernetes node upgrade can
	// move: containerd is set to the matrix version, and kubeadm and kubelet pull
	// in a newer cri-tools and kubernetes-cni when their minimum changes.
	UpgradeMovedPackages = []string{"containerd", "cri-tools", "kubernetes-cni"}

	// RuntimeHeldPackages are all the runtime and node service packages the node
	// install holds besides the Kubernetes trio.
	RuntimeHeldPackages = []string{"containerd", "haproxy", "dnsmasq", "wireguard-tools", "cri-tools", "kubernetes-cni"}
)

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
