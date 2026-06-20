package commons

import (
	"fmt"
)

// aptGet is apt-get with a bounded dpkg-lock wait (DPkg::Lock::Timeout) so a
// held lock makes apt wait a bounded time instead of blocking forever.
//
// IMPORTANT: each call site prepends `DEBIAN_FRONTEND=noninteractive` BEFORE
// `timeout`, never after it — `timeout`'s first argument is the program to run,
// so `timeout 300 DEBIAN_FRONTEND=... apt-get` makes timeout try to exec the
// env-assignment as a command and fail instantly (the apt-remove never runs).
const aptGet = "apt-get -o DPkg::Lock::Timeout=120 -y"

// Uninstall removes Kubernetes, Containerd and WireGuard and resets networking.
// When full is true it also clears RunOS configuration and certificates.
//
// Performance/robustness: every potentially-blocking step is bounded by
// `timeout` so a wedged kubeadm/systemctl/netplan/apt can't hang the whole
// uninstall, and ALL package removals are batched into a SINGLE non-interactive
// apt-get (previously five separate apt invocations, each able to stall for tens
// of seconds and contend on the dpkg lock — the cause of the long delays).
func Uninstall(full bool) {
	fmt.Println("Starting uninstallation process...")

	// Stop and reset Kubernetes. kubeadm reset can hang on a wedged container
	// runtime / etcd, so bound it.
	fmt.Println(ExecuteCommandGetResponse("timeout 120 kubeadm reset -f || true"))

	// Remove cluster configurations
	fmt.Println(ExecuteCommandGetResponse("rm -rf /etc/kubernetes || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /var/lib/kubelet || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /var/lib/etcd || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf ~/.kube || true"))

	// Remove CNI configurations
	fmt.Println(ExecuteCommandGetResponse("rm -rf /etc/cni || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /opt/cni || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /var/lib/cni || true"))

	// Stop and remove Wireguard (systemctl stop bounded — a stuck unit hangs it)
	fmt.Println(ExecuteCommandGetResponse("timeout 30 systemctl stop wg-quick@wg0 || true"))
	fmt.Println(ExecuteCommandGetResponse("timeout 30 systemctl disable wg-quick@wg0 || true"))
	fmt.Println(ExecuteCommandGetResponse("ip link delete wg0 || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /etc/wireguard || true"))

	// Clean up DNS configuration
	fmt.Println(ExecuteCommandGetResponse("timeout 30 systemctl stop dnsmasq || true"))
	fmt.Println(ExecuteCommandGetResponse("timeout 30 systemctl disable dnsmasq || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /etc/systemd/system/dnsmasq.service.d || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/dnsmasq.d/runos.conf || true"))

	// Remove netplan DNS override
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/netplan/99-runos-disable-dhcp-dns.yaml || true"))
	fmt.Println(ExecuteCommandGetResponse("timeout 30 netplan apply || true"))

	// Restore original systemd-resolved configuration if backup exists
	fmt.Println(ExecuteCommandGetResponse("if [ -f /etc/systemd/resolved.conf.runos-bak ]; then mv /etc/systemd/resolved.conf.runos-bak /etc/systemd/resolved.conf; fi || true"))
	fmt.Println(ExecuteCommandGetResponse("timeout 30 systemctl restart systemd-resolved || true"))
	fmt.Println(ExecuteCommandGetResponse("ln -sf /run/systemd/resolve/stub-resolv.conf /etc/resolv.conf || true"))
	fmt.Println(ExecuteCommandGetResponse("resolvectl flush-caches || systemd-resolve --flush-caches || true"))

	// Reset iptables
	fmt.Println(ExecuteCommandGetResponse("iptables -F || true"))
	fmt.Println(ExecuteCommandGetResponse("iptables -t nat -F || true"))
	fmt.Println(ExecuteCommandGetResponse("iptables -t mangle -F || true"))
	fmt.Println(ExecuteCommandGetResponse("iptables -X || true"))
	fmt.Println(ExecuteCommandGetResponse("iptables -P FORWARD ACCEPT || true"))

	// Reset IP forwarding and network configurations
	fmt.Println(ExecuteCommandGetResponse("sysctl net.bridge.bridge-nf-call-iptables=0 || true"))
	fmt.Println(ExecuteCommandGetResponse("sysctl net.bridge.bridge-nf-call-ip6tables=0 || true"))
	fmt.Println(ExecuteCommandGetResponse("sysctl net.ipv4.ip_forward=0 || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/sysctl.d/99-ipforward.conf || true"))

	// Clean kernel module config
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/modules-load.d/kubernetes.conf || true"))
	fmt.Println(ExecuteCommandGetResponse("sed -i '/br_netfilter/d' /etc/modules || true"))

	// Remove RunOS Managed entries from /etc/hosts
	fmt.Println(ExecuteCommandGetResponse("sed -i '/#RunOS Managed/d' /etc/hosts || true"))

	// Stop HAProxy before package removal (bounded)
	fmt.Println(ExecuteCommandGetResponse("timeout 30 systemctl stop haproxy || true"))
	fmt.Println(ExecuteCommandGetResponse("timeout 30 systemctl disable haproxy || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /var/run/haproxy || true"))

	// Unhold the held Kubernetes packages so they can be purged.
	fmt.Println(ExecuteCommandGetResponse("apt-mark unhold kubelet kubeadm kubectl || true"))

	// Remove ALL RunOS-installed packages in a SINGLE non-interactive apt-get
	// (was five separate, slow, lock-contending invocations — the long delay).
	// Bounded by the dpkg-lock timeout above plus an overall `timeout`.
	fmt.Println(ExecuteCommandGetResponse("DEBIAN_FRONTEND=noninteractive timeout 300 " + aptGet + " remove --purge dnsmasq kubeadm kubectl kubelet kubernetes-cni containerd wireguard wireguard-tools haproxy || true"))
	fmt.Println(ExecuteCommandGetResponse("DEBIAN_FRONTEND=noninteractive timeout 120 " + aptGet + " autoremove || true"))
	fmt.Println(ExecuteCommandGetResponse("apt-get clean || true"))

	// Remove Kubernetes apt repo
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/apt/sources.list.d/kubernetes.list || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/apt/keyrings/kubernetes-apt-keyring.gpg || true"))

	// Remove the RunOS Node Agent
	fmt.Println(ExecuteCommandGetResponse("rm -Rf /root/.runos"))
	fmt.Println(ExecuteCommandGetResponse("systemctl disable runos || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/systemd/system/runos.service || true"))
	if full {
		fmt.Println(ExecuteCommandGetResponse("systemctl daemon-reload || true"))
		fmt.Println(ExecuteCommandGetResponse("timeout 30 systemctl stop runos || true"))
	}

	fmt.Println("Uninstallation complete. You may need to reboot the system for all changes to take effect.")
}
