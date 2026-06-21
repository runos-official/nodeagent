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
	// Only reset if kubeadm is actually installed: an absent kubeadm means there
	// is nothing to reset (the host is already clean), NOT a failure. Without this
	// guard a re-run on a half-uninstalled box wedges forever — `timeout` can't
	// exec the missing kubeadm (exit 127), which marked this load-bearing step
	// failed and made `runos uninstall` report a partial uninstall on every retry.
	roslog.Print("Removing Kubernetes... ")
	critical("kubeadm reset", "if command -v kubeadm >/dev/null 2>&1; then timeout 120 kubeadm reset -f; fi")
	// Stop kubelet + the container runtime before wiping their data dirs so nothing
	// holds them open. kubeadm reset does this when present, but it may be absent on a
	// half-uninstalled box (the guard above skips it), so do it explicitly. Best-effort.
	step("timeout 30 systemctl stop kubelet || true")
	step("timeout 30 systemctl stop containerd || true")
	// Remove cluster + etcd data (load-bearing: leftover etcd data is the worst thing
	// to silently keep on an "uninstalled" node). Each wipe ASSERTS the target is
	// actually gone (`[ ! -e ... ]`): a bare `rm -rf` exits non-zero on a busy mount or
	// immutable file, and as a load-bearing step that would wedge the uninstall as a
	// permanent "partial uninstall" on every retry. /var/lib/kubelet can hold live
	// pod-volume mounts (SA-token / secret / emptyDir tmpfs), so lazy-unmount
	// everything under it (deepest first) before removing, or `rm` fails "device busy".
	critical("wipe /etc/kubernetes", "rm -rf /etc/kubernetes; [ ! -e /etc/kubernetes ]")
	critical("wipe /var/lib/kubelet", "awk '$2 ~ \"^/var/lib/kubelet\" {print $2}' /proc/mounts | sort -r | while read -r m; do umount -lf \"$m\" 2>/dev/null || true; done; rm -rf /var/lib/kubelet; [ ! -e /var/lib/kubelet ]")
	critical("wipe /var/lib/etcd", "rm -rf /var/lib/etcd; [ ! -e /var/lib/etcd ]")
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
	// Purge ALL RunOS-installed packages in a SINGLE non-interactive apt-get (was
	// five separate, slow, lock-contending invocations — the long delay). Bounded
	// by the dpkg-lock timeout above plus an overall `timeout`.
	//
	// Purge only the subset dpkg still tracks (installed or residual-config). Once
	// the k8s apt repo is removed — a best-effort step just below, which a PRIOR
	// partial uninstall may already have run — `apt-get remove kubeadm ...` fails
	// with "Unable to locate package" (exit 100) for the now-unknown names, which
	// wedged the uninstall on every retry. dpkg-query lists the present names; if
	// none remain there is nothing to purge and the step is a clean no-op.
	critical("purge packages", "pkgs=$(dpkg-query -W -f='${Package}\\n' dnsmasq kubeadm kubectl kubelet kubernetes-cni containerd wireguard wireguard-tools haproxy 2>/dev/null); if [ -n \"$pkgs\" ]; then DEBIAN_FRONTEND=noninteractive timeout 300 "+aptGet+" remove --purge $pkgs; else echo 'no RunOS packages present to purge'; fi")
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
		step("timeout 30 systemctl stop runos || true")
		step("systemctl daemon-reload || true")
		// Clear the node's RunOS identity: /etc/runos holds config.yaml (the NID)
		// plus the mTLS client cert + CA. The contract above ("when full is true it
		// also clears RunOS configuration and certificates") was never actually
		// implemented, so these survived every uninstall and the NEXT install was
		// BLOCKED by the already-registered preflight check. An uninstall that
		// DESTROYS the node must remove its identity too, leaving a clean slate.
		step("rm -rf /etc/runos || true")
	}

	if len(failed) > 0 {
		return fmt.Errorf("partial uninstall: %d load-bearing step(s) failed: %v", len(failed), failed)
	}
	return nil
}
