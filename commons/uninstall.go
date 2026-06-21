package commons

import (
	"fmt"

	"github.com/runos-official/nodeagent/roslog"
)

// aptGet is apt-get with a bounded dpkg-lock wait (DPkg::Lock::Timeout) so a
// held lock makes apt wait a bounded time instead of blocking forever.
//
// IMPORTANT: each call site prepends `DEBIAN_FRONTEND=noninteractive` BEFORE
// `timeout`, never after it — `timeout`'s first argument is the program to run,
// so `timeout 300 DEBIAN_FRONTEND=... apt-get` makes timeout try to exec the
// env-assignment as a command and fail instantly (the apt-remove never runs).
const aptGet = "apt-get -o DPkg::Lock::Timeout=120 -y"

// step runs a best-effort cleanup command. Its raw output is sent to the durable
// log only (via ExecuteCommandGetResponse -> roslog.I), never dumped to the
// terminal. Best-effort steps are not load-bearing: their failure does not make
// a wipe "partial", so we swallow whatever the `|| true`-guarded command returns.
func step(command string) {
	_ = ExecuteCommandGetResponse(command)
}

// criticalStep runs a load-bearing cleanup command and records whether it
// actually succeeded. Output and any error go to the durable log; the returned
// bool feeds the partial-uninstall accounting so a node that failed to reset
// Kubernetes / purge packages / wipe data does NOT report a clean uninstall.
func criticalStep(label, command string) bool {
	out, err := ExecuteCommandGetResponse2(command)
	if err != nil {
		roslog.E("Uninstall critical step failed", err, "step", label, "output", out)
		return false
	}
	return true
}

// Uninstall removes Kubernetes, Containerd and WireGuard and resets networking.
// When full is true it also clears RunOS configuration and certificates.
//
// It accumulates failures across load-bearing steps (kubeadm reset, package
// purge, etcd/data wipe) and returns a non-nil error if any of them failed, so
// callers can distinguish a partial wipe from a clean one. Best-effort cleanup
// (iptables flush, DNS restore, repo removal, ...) never fails the uninstall.
//
// Performance/robustness: every potentially-blocking step is bounded by
// `timeout` so a wedged kubeadm/systemctl/netplan/apt can't hang the whole
// uninstall, and ALL package removals are batched into a SINGLE non-interactive
// apt-get (previously five separate apt invocations, each able to stall for tens
// of seconds and contend on the dpkg lock — the cause of the long delays).
func Uninstall(full bool) error {
	roslog.Println("Starting uninstallation process...")

	// failed collects the labels of load-bearing steps that did not succeed so
	// the summary can name exactly what was left behind.
	var failed []string
	critical := func(label, command string) {
		if !criticalStep(label, command) {
			failed = append(failed, label)
		}
	}

	// --- Kubernetes (load-bearing) -----------------------------------------
	// kubeadm reset can hang on a wedged container runtime / etcd, so bound it.
	roslog.Print("Removing Kubernetes... ")
	critical("kubeadm reset", "timeout 120 kubeadm reset -f")
	// Remove cluster + etcd data (load-bearing: leftover etcd data is the worst
	// thing to silently keep on a "uninstalled" node).
	critical("wipe /etc/kubernetes", "rm -rf /etc/kubernetes")
	critical("wipe /var/lib/kubelet", "rm -rf /var/lib/kubelet")
	critical("wipe /var/lib/etcd", "rm -rf /var/lib/etcd")
	step("rm -rf ~/.kube || true")
	// CNI configurations (best-effort)
	step("rm -rf /etc/cni || true")
	step("rm -rf /opt/cni || true")
	step("rm -rf /var/lib/cni || true")
	roslog.Println("done")

	// --- WireGuard (best-effort) -------------------------------------------
	roslog.Print("Removing WireGuard... ")
	step("timeout 30 systemctl stop wg-quick@wg0 || true")
	step("timeout 30 systemctl disable wg-quick@wg0 || true")
	step("ip link delete wg0 || true")
	step("rm -rf /etc/wireguard || true")
	roslog.Println("done")

	// --- DNS / network reset (best-effort) ---------------------------------
	roslog.Print("Resetting network... ")
	// Clean up DNS configuration
	step("timeout 30 systemctl stop dnsmasq || true")
	step("timeout 30 systemctl disable dnsmasq || true")
	step("rm -rf /etc/systemd/system/dnsmasq.service.d || true")
	step("rm -f /etc/dnsmasq.d/runos.conf || true")
	// Remove netplan DNS override
	step("rm -f /etc/netplan/99-runos-disable-dhcp-dns.yaml || true")
	step("timeout 30 netplan apply || true")
	// Restore original systemd-resolved configuration if backup exists
	step("if [ -f /etc/systemd/resolved.conf.runos-bak ]; then mv /etc/systemd/resolved.conf.runos-bak /etc/systemd/resolved.conf; fi || true")
	step("timeout 30 systemctl restart systemd-resolved || true")
	step("ln -sf /run/systemd/resolve/stub-resolv.conf /etc/resolv.conf || true")
	step("resolvectl flush-caches || systemd-resolve --flush-caches || true")
	// Reset iptables
	step("iptables -F || true")
	step("iptables -t nat -F || true")
	step("iptables -t mangle -F || true")
	step("iptables -X || true")
	step("iptables -P FORWARD ACCEPT || true")
	// Reset IP forwarding and network configurations
	step("sysctl net.bridge.bridge-nf-call-iptables=0 || true")
	step("sysctl net.bridge.bridge-nf-call-ip6tables=0 || true")
	step("sysctl net.ipv4.ip_forward=0 || true")
	step("rm -f /etc/sysctl.d/99-ipforward.conf || true")
	// Clean kernel module config
	step("rm -f /etc/modules-load.d/kubernetes.conf || true")
	step("sed -i '/br_netfilter/d' /etc/modules || true")
	// Remove RunOS Managed entries from /etc/hosts
	step("sed -i '/#RunOS Managed/d' /etc/hosts || true")
	roslog.Println("done")

	// --- HAProxy (best-effort) ---------------------------------------------
	step("timeout 30 systemctl stop haproxy || true")
	step("timeout 30 systemctl disable haproxy || true")
	step("rm -rf /var/run/haproxy || true")

	// --- Packages (load-bearing) -------------------------------------------
	roslog.Print("Removing packages... ")
	// Unhold the held Kubernetes packages so they can be purged (best-effort).
	step("apt-mark unhold kubelet kubeadm kubectl || true")
	// Remove ALL RunOS-installed packages in a SINGLE non-interactive apt-get
	// (was five separate, slow, lock-contending invocations — the long delay).
	// Bounded by the dpkg-lock timeout above plus an overall `timeout`.
	critical("purge packages", "DEBIAN_FRONTEND=noninteractive timeout 300 "+aptGet+" remove --purge dnsmasq kubeadm kubectl kubelet kubernetes-cni containerd wireguard wireguard-tools haproxy")
	step("DEBIAN_FRONTEND=noninteractive timeout 120 " + aptGet + " autoremove || true")
	step("apt-get clean || true")
	// Remove Kubernetes apt repo (best-effort)
	step("rm -f /etc/apt/sources.list.d/kubernetes.list || true")
	step("rm -f /etc/apt/keyrings/kubernetes-apt-keyring.gpg || true")
	roslog.Println("done")

	// --- RunOS Node Agent (best-effort) ------------------------------------
	step("rm -Rf /root/.runos || true")
	step("systemctl disable runos || true")
	step("rm -f /etc/systemd/system/runos.service || true")
	if full {
		step("systemctl daemon-reload || true")
		step("timeout 30 systemctl stop runos || true")
	}

	if len(failed) > 0 {
		return fmt.Errorf("partial uninstall: %d load-bearing step(s) failed: %v", len(failed), failed)
	}
	return nil
}
