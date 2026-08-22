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

// kubeletLazyUnmount lazy-unmounts everything under /var/lib/kubelet, deepest first. Lazy,
// because a mount held by an orphaned process still detaches from the tree. Run under
// `timeout -k 5 60 sh -c '...'`; it contains no single quote on purpose.
const kubeletLazyUnmount = `awk "\$2 ~ /^\/var\/lib\/kubelet/ {print \$2}" /proc/mounts | sort -r | while read -r m; do umount -lf "$m" 2>/dev/null || true; done`

// networkdUnitDir holds the systemd-networkd units RunOS writes. Only a parameter of
// vmGroupBridgeCleanupSteps so the test can drive the real removal against a fixture tree.
const networkdUnitDir = "/etc/systemd/network"

// Where conductor's 076-vm-group-bridge puts the VM group SEGMENT FIREWALL. Parameters of
// vmGroupFirewallCleanupSteps for the same reason: so its test can drive the real removal.
const (
	segmentFirewallConfDir = "/etc/runos/vm-group-firewall"
	segmentFirewallApplier = "/usr/local/sbin/runos-vm-group-firewall"
	segmentFirewallUnitDir = "/etc/systemd/system"
)

// vmGroupBridgeCleanupSteps removes the VM group pool bridges this node carries.
//
// Conductor's script 076-vm-group-bridge persists one bridge per VM group as
// 90-rvg<gid>.netdev plus 90-rvg<gid>.network, and nothing in the uninstall knew about them.
// MEASURED on ftb1 2026-08-16: after two full resets the box still had both unit files and the
// live rvg* links with the groups' gateway addresses on them, so a re-provisioned node came up
// owning segments for groups that no longer existed.
//
// Scoped to the 90-rvg prefix on purpose. wg0 and the cilium interfaces live in the same
// directory and on the same link table, and a wider glob or an unfiltered link loop would take
// the node off its own overlay to clean up a VM bridge.
// runtimeDataWipeSteps removes the container runtime's DATA, and only its data.
//
// MEASURED on ftb2 2026-08-22, and it blocked a rebuild. After `clusters reset` uninstalled
// Kubernetes and rebooted the box "back to bare", /var/lib/containerd still held 43 GB on a
// 63 GB root, taking it to 80% full with containerd itself inactive. The immediate rejoin was
// refused by preflight [disk-space], so a machine that had been a healthy node ten minutes
// earlier could not be reinstalled. ftb1 carried 32 GB of the same and only escaped because its
// root is 274 GB, which is exactly why this went unnoticed: it only bites the smaller disk.
//
// The uninstall already stops containerd and already wipes /etc/kubernetes, /var/lib/kubelet,
// /var/lib/etcd and the CNI directories. The runtime's own image store was simply missing from
// that list.
//
// BEST-EFFORT AND BOUNDED, deliberately, unlike the etcd and kubelet wipes above. Those assert
// the target is gone because leftover cluster state is dangerous. This one is only litter: if a
// reparented shim still holds a snapshot mount, failing the whole uninstall would turn a disk
// space problem into a permanent "partial uninstall" on every retry, which is worse. When it does
// fail the next preflight says [disk-space] in plain words, which is a recoverable outcome.
//
// Data only. /etc/containerd and the packages stay, because a reinstall reuses them and only
// needs the image store gone.
func runtimeDataWipeSteps() []string {
	return []string{
		"timeout -k 5 120 rm -rf /var/lib/containerd || true",
	}
}

func vmGroupBridgeCleanupSteps(unitDir string) []string {
	return []string{
		fmt.Sprintf("rm -f %s/90-rvg*.netdev %s/90-rvg*.network || true", unitDir, unitDir),
		// The name is field 2 of `ip -o link show`; a link with a peer reads as name@peer, so the
		// suffix is cut before matching. grep is anchored so only the pool bridges match.
		//
		// Bounded by `timeout 30` like every other step that talks to the kernel: an `ip` wedged
		// on a stuck netlink socket would otherwise hold the whole uninstall open with no way
		// out. Run under `timeout 30 sh -c '...'`, so the body carries no single quote: awk's
		// program is double-quoted and its $2 escaped from the outer shell.
		`timeout 30 sh -c 'for l in $(ip -o link show 2>/dev/null | awk -F": " "{print \$2}" | ` +
			`cut -d@ -f1 | grep "^rvg" || true); do ip link set "$l" down || true; ` +
			`ip link delete "$l" || true; done' || true`,
		"timeout 30 networkctl reload || true",
	}
}

// vmGroupFirewallCleanupSteps removes the VM group segment firewall this node carries.
//
// Conductor's 076-vm-group-bridge installs three things a node keeps by itself: a conf per group in
// /etc/runos/vm-group-firewall, an applier in /usr/local/sbin, and a oneshot unit that re-applies
// the rules at boot. It also installs iptables chains jumped to from mangle PREROUTING, filter
// INPUT (IPv4 and IPv6), nat POSTROUTING and nat PREROUTING, and for an operator-assigned VM address
// (077-vm-address-binding, goal 30) it holds that address on the WAN interface and records it in
// .held-addresses in the same conf dir. For a machine with customer firewall rules
// (078-vm-firewall, goal 30) it also installs a RUNOS-VMFW dispatch chain jumped to from MANGLE
// FORWARD plus two RUNOS-VMFW-<vmid>-* chains, and records their names in .vmfw-chains. It was
// filter FORWARD until G30-F16 (2026-08-19), so both tables are swept.
//
// MEASURED on 2026-08-17, immediately after writing the thing: a full cluster reset left the unit
// ENABLED, the applier in place and both chains installed on every host, on boxes the reset had
// otherwise returned to bare. That is the same shape as the bridge gap above, found the same way,
// two rounds later. The rule this exists to enforce: anything RunOS puts on a node gets its removal
// written in the same change, and the uninstall is the one place that can be checked.
//
// The chains are torn down BEFORE the files, so a boot that races the uninstall cannot re-apply
// from a conf that is about to disappear. Every step is scoped to the RUNOS-VMGRP names and the
// RunOS paths: a wider flush would take cilium's and kube-proxy's chains with it.
func vmGroupFirewallCleanupSteps(confDir, applier, unitDir string) []string {
	return []string{
		"timeout 30 systemctl disable --now runos-vm-group-firewall.service || true",
		// mangle first, then filter, on both address families. `-D` on an absent jump and `-X` on
		// an absent chain both fail harmlessly, which is why each is guarded.
		"timeout 30 sh -c 'iptables -t mangle -D PREROUTING -j RUNOS-VMGRP-PRE 2>/dev/null; " +
			"iptables -t mangle -F RUNOS-VMGRP-PRE 2>/dev/null; " +
			"iptables -t mangle -X RUNOS-VMGRP-PRE 2>/dev/null' || true",
		"timeout 30 sh -c 'for b in iptables ip6tables; do " +
			"$b -D INPUT -j RUNOS-VMGRP-IN 2>/dev/null; " +
			"$b -F RUNOS-VMGRP-IN 2>/dev/null; " +
			"$b -X RUNOS-VMGRP-IN 2>/dev/null; done' || true",
		// The FORWARD chain an earlier version of the fence used. Removed here too, because a node
		// provisioned before the hook moved still carries it.
		"timeout 30 sh -c 'iptables -D FORWARD -j RUNOS-VMGRP-FWD 2>/dev/null; " +
			"iptables -F RUNOS-VMGRP-FWD 2>/dev/null; " +
			"iptables -X RUNOS-VMGRP-FWD 2>/dev/null' || true",
		// The nat POSTROUTING chain that masquerades pool egress (goal 30,
		// guest-egress-without-the-pod-nic). Same rule as the fence chains: whatever RunOS installs on
		// a node, its removal is written in the same change, so a reset leaves the box bare.
		"timeout 30 sh -c 'iptables -t nat -D POSTROUTING -j RUNOS-VMGRP-NAT 2>/dev/null; " +
			"iptables -t nat -F RUNOS-VMGRP-NAT 2>/dev/null; " +
			"iptables -t nat -X RUNOS-VMGRP-NAT 2>/dev/null' || true",
		// The assigned addresses the applier put on the WAN interface for onlink bindings, recorded
		// as `WAN IP` lines in .held-addresses. Released BEFORE the DNAT chain below goes (unit-7
		// review): the host must stop answering for the address before it stops forwarding it, or for
		// that window the internet's packets for a VM land on the host's own sshd. And before the conf
		// dir goes, because the file is the only record of which addresses on the interface are RunOS's.
		// Read line by line and never globbed, so nothing but the listed pairs is touched.
		fmt.Sprintf("timeout 30 sh -c 'f=%s/.held-addresses; [ -f \"$f\" ] || exit 0; "+
			"while read -r wan addr; do [ -n \"$wan\" ] && [ -n \"$addr\" ] || continue; "+
			"ip addr del \"$addr/32\" dev \"$wan\" 2>/dev/null; done < \"$f\"' || true", confDir),
		// The nat PREROUTING chain that DNATs an operator-assigned address to a VM's pool address
		// (goal 30, associate-and-disassociate). Same rule again.
		"timeout 30 sh -c 'iptables -t nat -D PREROUTING -j RUNOS-VMGRP-DNAT 2>/dev/null; " +
			"iptables -t nat -F RUNOS-VMGRP-DNAT 2>/dev/null; " +
			"iptables -t nat -X RUNOS-VMGRP-DNAT 2>/dev/null' || true",
		// The `-N` shadow chains the applier builds into and renames on a clean run. On a clean node
		// they are already renamed to the stable names above, so these are usually no-ops; they only
		// exist when a build failed mid-swap, and a reset must still leave the box bare.
		"timeout 30 sh -c 'iptables -t mangle -F RUNOS-VMGRP-PRE-N 2>/dev/null; " +
			"iptables -t mangle -X RUNOS-VMGRP-PRE-N 2>/dev/null; " +
			"iptables -t nat -F RUNOS-VMGRP-NAT-N 2>/dev/null; " +
			"iptables -t nat -X RUNOS-VMGRP-NAT-N 2>/dev/null; " +
			"iptables -t nat -F RUNOS-VMGRP-DNAT-N 2>/dev/null; " +
			"iptables -t nat -X RUNOS-VMGRP-DNAT-N 2>/dev/null; " +
			"for b in iptables ip6tables; do $b -F RUNOS-VMGRP-IN-N 2>/dev/null; $b -X RUNOS-VMGRP-IN-N 2>/dev/null; done' || true",
		// The CUSTOMER firewall (goal 30, customer-firewall-rules): a dispatch chain jumped to from
		// filter FORWARD, plus two chains per machine that had rules on this node. Their names carry
		// the machine's vmid, so they cannot be listed here the way the fixed RUNOS-VMGRP names can;
		// `iptables -S` is asked which ones exist instead. Same rule as every chain above: whatever
		// RunOS installs on a node, its removal is written in the same change, so a reset leaves the
		// box bare. The FORWARD jump goes FIRST, so nothing is jumping into a chain being deleted.
		// FLUSHED IN ONE PASS AND DELETED IN A SECOND, not both in one loop. `iptables -S` lists the
		// chains in an arbitrary order, and `-X` refuses a chain another chain still jumps to, so a
		// per-machine chain reached before the dispatch chain would survive the run. Flushing the
		// dispatch chain removes every jump into the per-machine chains, and then all of them delete.
		// BOTH JUMPS GO: the applier hooks FORWARD to the `-N` SHADOW first and renames it to the
		// stable name on a clean swap, so a run that died mid-swap leaves `-j RUNOS-VMFW-N` in FORWARD.
		// Deleting only the stable jump left that one behind, and `-X` then refused the chain it names.
		// BOTH TABLES. The customer firewall moved from filter to MANGLE (G30-F16, 2026-08-19),
		// because in filter it hung under cilium's `-i lxc+ -j ACCEPT`, which ends the filter walk,
		// so a pod on the machine's own host bypassed the customer's rules entirely. An uninstall
		// must clear wherever the chains actually are, and a node uninstalled after an upgrade can
		// still carry the old FILTER copies, so both are swept unconditionally. Every command is a
		// no-op where the chains are absent.
		"timeout 30 sh -c 'for t in filter mangle; do " +
			"iptables -t $t -D FORWARD -j RUNOS-VMFW 2>/dev/null; " +
			"iptables -t $t -D FORWARD -j RUNOS-VMFW-N 2>/dev/null; " +
			"cs=$(iptables -t $t -S 2>/dev/null | awk \"/^-N RUNOS-VMFW/{print \\$2}\"); " +
			"for c in $cs; do iptables -t $t -F \"$c\" 2>/dev/null; done; " +
			"for c in $cs; do iptables -t $t -X \"$c\" 2>/dev/null; done; " +
			"done' || true",
		fmt.Sprintf("rm -f %s/runos-vm-group-firewall.service || true", unitDir),
		fmt.Sprintf("rm -f %s %s.tmp || true", applier, applier),
		fmt.Sprintf("rm -rf %s || true", confDir),
		"timeout 30 systemctl daemon-reload || true",
	}
}

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
	// Release the CSI block-device mounts BEFORE kubeadm reset, or the reset fails on ANY node
	// that has ever hosted a virtual machine (goal 23, F9). Measured on all four campaign hosts:
	//
	//   Uninstall critical step failed  step="kubeadm reset"
	//     error: ... failed to unmount ".../csi/volumeDevices/pvc-.../dev/..." : device or
	//     resource busy
	//
	// Deterministic, not a race. kubeadm's own unmount is a plain umount and cannot release a
	// bind mount whose backing DRBD device is still open, so the whole uninstall reported
	// partial, the caller correctly refused to reboot, and the node was then left with
	// kube-apiserver still LISTENING on 6443 and cilium-agent still running, because stopping
	// containerd does not stop its reparented shim children.
	//
	// Best-effort and ordered: stop the kubelet so nothing re-attaches a volume as fast as it is
	// detached, drop the DRBD devices that hold the bind mounts open, then lazy-unmount deepest
	// first. Lazy, because a mount held by an orphaned process still detaches from the tree.
	//
	// Every `timeout` here carries -k (goal 23 review, F9-b): a process that ignores TERM,
	// which is exactly what a wedged kubeadm or drbdsetup does, would otherwise outlive the
	// timeout and hang the uninstall. The umount loop is bounded too; a stuck umount on a
	// dead DRBD backing device blocks forever without it.
	step("timeout 30 systemctl stop kubelet || true")
	step("if command -v drbdsetup >/dev/null 2>&1; then timeout -k 5 30 drbdsetup down all || true; fi")
	step("timeout -k 5 60 sh -c '" + kubeletLazyUnmount + "' || true")
	critical("kubeadm reset", "if command -v kubeadm >/dev/null 2>&1; then timeout -k 10 120 kubeadm reset -f; fi")
	// Kill the pod sandboxes and shims BEFORE containerd stops (goal 23 review, F9-a).
	// Stopping containerd does not stop its shim children: they are reparented and keep
	// kube-apiserver and cilium-agent running, listening on 6443 and holding the CNI
	// interfaces, until a reboot. Measured on all four campaign hosts, still serving on 6443
	// two hours after a partial uninstall. Best-effort: kubeadm reset usually did this
	// already, and a missing crictl on a half-uninstalled box is not a failure.
	step("if command -v crictl >/dev/null 2>&1; then timeout -k 5 60 crictl -r unix:///run/containerd/containerd.sock rmp -fa || true; fi")
	step("pkill -9 -f '[c]ontainerd-shim.*-namespace k8s.io' || true")
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
	critical("wipe /var/lib/kubelet", "timeout -k 5 60 sh -c '"+kubeletLazyUnmount+"' || true; rm -rf /var/lib/kubelet; [ ! -e /var/lib/kubelet ]")
	critical("wipe /var/lib/etcd", "rm -rf /var/lib/etcd; [ ! -e /var/lib/etcd ]")
	step("rm -rf ~/.kube || true")
	// CNI configurations (best-effort)
	step("rm -rf /etc/cni || true")
	step("rm -rf /opt/cni || true")
	step("rm -rf /var/lib/cni || true")
	// The container runtime's image store. containerd was stopped a few lines above, so this is
	// orphaned data by the time it runs. See runtimeDataWipeSteps for what it cost to leave it.
	for _, s := range runtimeDataWipeSteps() {
		step(s)
	}
	roslog.Println("done")

	// --- WireGuard (best-effort) -------------------------------------------
	roslog.Print("Removing WireGuard... ")
	step("timeout 30 systemctl stop wg-quick@wg0 || true")
	step("timeout 30 systemctl disable wg-quick@wg0 || true")
	step("ip link delete wg0 || true")
	step("rm -rf /etc/wireguard || true")
	// The RunOS instance unit and the older drop-in (goal 23 review): both are RunOS files
	// and both survived every uninstall, so a re-provisioned box carried a wg0 unit for a
	// tunnel that no longer existed.
	step("rm -f " + WgQuickUnitPath + " || true")
	step("rm -rf " + wgQuickDropInDir + " || true")
	step("systemctl daemon-reload || true")
	// AND CLEAR THE FAILED STATE, after the unit file is gone and the reload has run. systemd keeps
	// a failed unit in its list even once the fragment is deleted, so `systemctl --failed` showed
	// `wg-quick@wg0.service not-found failed` and `systemctl is-system-running` answered DEGRADED on
	// a box RunOS had just wiped clean. Measured on ftb1 2026-08-20, on the very uninstall that was
	// meant to hand back a pristine machine. Same class as G28-F2, on the way out instead of in.
	step("systemctl reset-failed wg-quick@wg0 || true")
	roslog.Println("done")

	// --- VM group pool bridges (best-effort) -------------------------------
	// After the WireGuard teardown, and it reloads separately: these are networkd
	// units, so they need `networkctl reload`, which the WireGuard block's
	// `systemctl daemon-reload` does not do.
	roslog.Print("Removing VM group pool bridges... ")
	for _, s := range vmGroupBridgeCleanupSteps(networkdUnitDir) {
		step(s)
	}
	roslog.Println("done")

	// --- VM group segment firewall (best-effort) ---------------------------
	// After the bridges, because the rules name those bridges: tearing the fence down first would
	// leave a window where the segments exist and nothing fences them.
	roslog.Print("Removing VM group segment firewall... ")
	for _, s := range vmGroupFirewallCleanupSteps(segmentFirewallConfDir, segmentFirewallApplier, segmentFirewallUnitDir) {
		step(s)
	}
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
