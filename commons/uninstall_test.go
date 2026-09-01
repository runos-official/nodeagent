package commons

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// R4, measured on a lab box 2026-08-16: after `runos uninstall` (and after the control-plane-driven
// UNINSTALL_NODE) the node still carried /etc/systemd/network/90-rvg<gid>.netdev and .network
// plus the live rvg* links with their gateway addresses. A re-provisioned box therefore came up
// owning VM group pool bridges for groups that no longer existed.

// runStep executes one cleanup step with sh, as Uninstall does.
func runStep(t *testing.T, step string, extraPath string) string {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", step)
	if extraPath != "" {
		cmd.Env = append(os.Environ(), "PATH="+extraPath+":"+os.Getenv("PATH"))
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("step %q failed: %v (%s)", step, err, out)
	}
	return string(out)
}

func TestVmGroupBridgeCleanupSteps_RemovesOnlyTheRvgUnits(t *testing.T) {
	dir := t.TempDir()
	keep := []string{"90-wg0.network", "10-runos-uplink.network", "90-rvgnot.txt"}
	remove := []string{"90-rvg7.netdev", "90-rvg7.network", "90-rvg12.netdev", "90-rvg12.network"}
	for _, name := range append(append([]string{}, keep...), remove...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("fixture write failed: %v", err)
		}
	}

	steps := vmGroupBridgeCleanupSteps(dir)
	if len(steps) == 0 {
		t.Fatal("expected at least one cleanup step")
	}
	runStep(t, steps[0], "")

	for _, name := range remove {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed, stat err = %v", name, err)
		}
	}
	for _, name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s must survive: %v", name, err)
		}
	}
}

func TestVmGroupBridgeCleanupSteps_DeletesOnlyTheRvgLinks(t *testing.T) {
	// A fake `ip` reports the shape of a real VM host and records every call, so the test proves
	// the loop deletes the pool bridges and NEVER wg0 or a cilium interface.
	binDir := t.TempDir()
	calls := filepath.Join(binDir, "calls.log")
	fake := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + calls + "\n" +
		"if [ \"$1\" = \"-o\" ]; then\n" +
		"  echo '1: lo: <LOOPBACK>'\n" +
		"  echo '2: eth0: <BROADCAST>'\n" +
		"  echo '3: wg0: <POINTOPOINT>'\n" +
		"  echo '4: cilium_host@cilium_net: <BROADCAST>'\n" +
		"  echo '5: rvg7: <BROADCAST>'\n" +
		"  echo '6: rvg12: <BROADCAST>'\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "ip"), []byte(fake), 0o755); err != nil {
		t.Fatalf("fake ip write failed: %v", err)
	}

	// A stand-in for `timeout`, which macOS does not ship. It drops the duration
	// and runs the rest, so the step is exercised exactly as written.
	fakeTimeout := "#!/bin/sh\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "timeout"), []byte(fakeTimeout), 0o755); err != nil {
		t.Fatalf("fake timeout write failed: %v", err)
	}

	steps := vmGroupBridgeCleanupSteps(t.TempDir())
	if len(steps) < 2 {
		t.Fatalf("expected a link-deletion step, got %d steps", len(steps))
	}
	runStep(t, steps[1], binDir)

	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("the fake ip recorded nothing: %v", err)
	}
	log := string(raw)
	for _, want := range []string{"link delete rvg7", "link delete rvg12"} {
		if !strings.Contains(log, want) {
			t.Errorf("expected %q among the ip calls, got:\n%s", want, log)
		}
	}
	for _, forbidden := range []string{"delete wg0", "delete cilium_host", "delete eth0", "delete lo"} {
		if strings.Contains(log, forbidden) {
			t.Errorf("the cleanup must never run %q, got:\n%s", forbidden, log)
		}
	}
}

// TestVmGroupBridgeCleanupSteps_BoundsTheLinkLoop: every other command in the
// uninstall that talks to the kernel or to systemd runs under `timeout 30`. The
// link loop did not, so an `ip` that wedged on a stuck netlink socket held the
// whole uninstall open with no way out, on a node the operator has already been
// told is being wiped.
func TestVmGroupBridgeCleanupSteps_BoundsTheLinkLoop(t *testing.T) {
	steps := vmGroupBridgeCleanupSteps("/etc/systemd/network")
	if len(steps) < 2 {
		t.Fatalf("expected a link-deletion step, got %d steps", len(steps))
	}
	if !strings.HasPrefix(steps[1], "timeout 30 ") {
		t.Errorf("the link-deletion loop must be bounded like the other steps, got: %s", steps[1])
	}
}

func TestVmGroupBridgeCleanupSteps_ReloadsNetworkd(t *testing.T) {
	// Removing the units without a reload leaves networkd holding the old configuration, so the
	// bridge comes back the next time anything touches the interface.
	joined := strings.Join(vmGroupBridgeCleanupSteps("/etc/systemd/network"), "\n")
	if !strings.Contains(joined, "networkctl reload") {
		t.Errorf("expected a networkctl reload, got:\n%s", joined)
	}
}

// The VM group SEGMENT FIREWALL, which conductor's 076-vm-group-bridge installs onto a node: a conf
// per group, an applier, a boot unit, and iptables chains on both address families.
//
// MEASURED 2026-08-17, immediately after that fence was written: a full cluster reset left the unit
// enabled, the applier in place and both chains installed on every host, on boxes the reset had
// otherwise returned to bare. Same shape as the pool-bridge gap two rounds earlier. Anything RunOS
// puts on a node needs its removal written in the same change.

func TestVmGroupFirewallCleanupSteps_RemovesTheFilesItOwns(t *testing.T) {
	root := t.TempDir()
	confDir := filepath.Join(root, "conf")
	unitDir := filepath.Join(root, "units")
	sbin := filepath.Join(root, "sbin")
	for _, d := range []string{confDir, unitDir, sbin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("fixture dir failed: %v", err)
		}
	}
	applier := filepath.Join(sbin, "runos-vm-group-firewall")
	unit := filepath.Join(unitDir, "runos-vm-group-firewall.service")
	// A .tmp is what an interrupted atomic write leaves; it must go too.
	files := []string{applier, applier + ".tmp", unit,
		filepath.Join(confDir, "rvgaaa.conf"), filepath.Join(confDir, "rvgzzz.conf")}
	for _, f := range files {
		if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("fixture write failed: %v", err)
		}
	}
	// A neighbour that must survive: the cleanup is scoped to what RunOS owns.
	keep := filepath.Join(unitDir, "some-other.service")
	if err := os.WriteFile(keep, []byte("x\n"), 0o644); err != nil {
		t.Fatalf("fixture write failed: %v", err)
	}

	for _, s := range vmGroupFirewallCleanupSteps(confDir, applier, unitDir) {
		runStep(t, s, "")
	}

	for _, f := range files {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed, stat err = %v", f, err)
		}
	}
	if _, err := os.Stat(confDir); !os.IsNotExist(err) {
		t.Errorf("the conf directory should have been removed, stat err = %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a unit RunOS does not own was removed: %v", err)
	}
}

func TestVmGroupFirewallCleanupSteps_TearsDownEveryChainItInstalls(t *testing.T) {
	// Both address families, both hooks, and the FORWARD chain an earlier version of the fence
	// used, because a node provisioned before the hook moved still carries it.
	joined := strings.Join(vmGroupFirewallCleanupSteps("/etc/runos/vm-group-firewall",
		"/usr/local/sbin/runos-vm-group-firewall", "/etc/systemd/system"), "\n")

	for _, want := range []string{
		"iptables -t mangle -D PREROUTING -j RUNOS-VMGRP-PRE",
		"iptables -t mangle -X RUNOS-VMGRP-PRE",
		"$b -D INPUT -j RUNOS-VMGRP-IN",
		"$b -X RUNOS-VMGRP-IN",
		"ip6tables",
		"iptables -D FORWARD -j RUNOS-VMGRP-FWD",
		// The nat POSTROUTING chain that masquerades pool egress (goal 30).
		"iptables -t nat -D POSTROUTING -j RUNOS-VMGRP-NAT",
		"iptables -t nat -X RUNOS-VMGRP-NAT",
		// The nat PREROUTING chain that DNATs an assigned address to a VM (goal 30), and its shadow.
		"iptables -t nat -D PREROUTING -j RUNOS-VMGRP-DNAT",
		"iptables -t nat -X RUNOS-VMGRP-DNAT",
		"iptables -t nat -X RUNOS-VMGRP-DNAT-N",
		"systemctl disable --now runos-vm-group-firewall.service",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("cleanup does not tear down %q", want)
		}
	}
}

func TestVmGroupFirewallCleanupSteps_NeverFlushesAChainItDoesNotOwn(t *testing.T) {
	// A bare `-F` or a flush of a builtin would take cilium's and kube-proxy's rules with it, and
	// the node would lose its own overlay to clean up a VM fence.
	for _, s := range vmGroupFirewallCleanupSteps("/etc/runos/vm-group-firewall",
		"/usr/local/sbin/runos-vm-group-firewall", "/etc/systemd/system") {
		// The builtins, by name. `-F RUNOS-VMGRP-*` is the point of the step, so the check is on
		// which chain is named rather than on the flag.
		for _, builtin := range []string{"PREROUTING", "INPUT", "FORWARD", "OUTPUT", "POSTROUTING"} {
			for _, flag := range []string{"-F ", "-X "} {
				if strings.Contains(s, flag+builtin) {
					t.Errorf("step %ss the builtin chain %s, which is not RunOS's to touch: %s", flag, builtin, s)
				}
			}
		}
	}
}

func TestVmGroupFirewallCleanupSteps_TearsDownRulesBeforeRemovingConfs(t *testing.T) {
	// Otherwise a boot that races the uninstall re-applies from a conf that is about to vanish.
	steps := vmGroupFirewallCleanupSteps("/etc/runos/vm-group-firewall",
		"/usr/local/sbin/runos-vm-group-firewall", "/etc/systemd/system")
	lastChain, firstRemove := -1, len(steps)
	for i, s := range steps {
		if strings.Contains(s, "RUNOS-VMGRP") {
			lastChain = i
		}
		if strings.HasPrefix(s, "rm ") && firstRemove == len(steps) {
			firstRemove = i
		}
	}
	if lastChain == -1 || firstRemove == len(steps) {
		t.Fatal("expected both chain teardown and file removal steps")
	}
	if lastChain > firstRemove {
		t.Errorf("chain teardown (step %d) must precede file removal (step %d)", lastChain, firstRemove)
	}
}

func TestVmGroupFirewallCleanupSteps_ReleasesTheHeldAddressesBeforeRemovingTheRecord(t *testing.T) {
	// 077-vm-address-binding (goal 30, associate-and-disassociate) holds an operator-assigned address
	// on the WAN interface for an onlink binding and records `WAN IP` in .held-addresses. The address
	// does not go with the conf dir: left on the interface after the DNAT is torn down, the host
	// itself answers on a VM's public address. The file is the only record of which addresses are
	// RunOS's, so it is read BEFORE the dir is removed and nothing else on the interface is touched.
	root := t.TempDir()
	confDir := filepath.Join(root, "conf")
	bin := filepath.Join(root, "bin")
	for _, d := range []string{confDir, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("fixture dir failed: %v", err)
		}
	}
	held := filepath.Join(confDir, ".held-addresses")
	if err := os.WriteFile(held, []byte("eth0 203.0.113.10\neth1 203.0.113.11\n\n"), 0o644); err != nil {
		t.Fatalf("fixture write failed: %v", err)
	}
	log := filepath.Join(root, "ip.log")
	fakeIP := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte(fakeIP), 0o755); err != nil {
		t.Fatalf("fixture write failed: %v", err)
	}
	// A stand-in for `timeout`, which macOS does not ship; it drops the duration and runs the rest.
	if err := os.WriteFile(filepath.Join(bin, "timeout"), []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatalf("fixture write failed: %v", err)
	}

	steps := vmGroupFirewallCleanupSteps(confDir, filepath.Join(root, "applier"), filepath.Join(root, "units"))
	for _, s := range steps {
		runStep(t, s, bin)
	}

	out, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the fake ip was never called, so no held address was released: %v", err)
	}
	for _, want := range []string{"addr del 203.0.113.10/32 dev eth0", "addr del 203.0.113.11/32 dev eth1"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("held address not released, want %q in:\n%s", want, out)
		}
	}
	// Two addresses, two calls: nothing else on the interface is touched.
	if n := strings.Count(string(out), "addr del"); n != 2 {
		t.Errorf("expected exactly 2 address releases, got %d:\n%s", n, out)
	}
	// ORDER: the addresses are released BEFORE the DNAT chain goes (unit-7 review). The other way
	// round, the host answers for a VM's address while nothing forwards it: its own sshd on the
	// guest's public address for that window.
	heldIdx, dnatIdx := -1, -1
	for i, st := range steps {
		if heldIdx < 0 && strings.Contains(st, ".held-addresses") {
			heldIdx = i
		}
		if dnatIdx < 0 && strings.Contains(st, "-X RUNOS-VMGRP-DNAT ") {
			dnatIdx = i
		}
	}
	if heldIdx < 0 || dnatIdx < 0 || heldIdx > dnatIdx {
		t.Errorf("held addresses (step %d) must be released before the DNAT chain is removed (step %d)", heldIdx, dnatIdx)
	}
	if _, err := os.Stat(confDir); !os.IsNotExist(err) {
		t.Errorf("the conf directory (and its held file) should have been removed, stat err = %v", err)
	}
}

// THE CUSTOMER FIREWALL (goal 30, customer-firewall-rules). 078-vm-firewall gives a machine two
// chains whose names carry its vmid, jumped to from a RUNOS-VMFW dispatch chain in filter FORWARD.
// Those names are not knowable here, so the cleanup asks `iptables -S` which ones exist. Same rule
// as every other chain in this file: whatever RunOS installs on a node, its removal is written in
// the same change, or a reset leaves a box that is not bare.

func TestVmGroupFirewallCleanupSteps_RemovesTheCustomerFirewallChains(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("fixture dir failed: %v", err)
	}
	log := filepath.Join(root, "iptables.log")
	// A fake iptables that answers `-S` with one machine's chains, the dispatch chain and a leftover
	// shadow, plus chains RunOS does not own. Everything else it is asked to do is recorded.
	// The cleanup now sweeps BOTH tables (G30-F16 moved the customer firewall to mangle), so every
	// call carries `-t <table>` and the fixture must look past it to find -S. Keying on $1 alone
	// silently listed nothing and every assertion below failed at once, which is how this was caught.
	fake := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + log + "\n" +
		"for a in \"$@\"; do case \"$a\" in -S) printf '%s\\n' " +
		"'-N CILIUM_FORWARD' '-N RUNOS-VMFW' '-N RUNOS-VMFW-N' '-N RUNOS-VMFW-vm1abc-IN' '-N RUNOS-VMFW-vm1abc-OUT' " +
		"'-N RUNOS-VMFW-vm2def-IN-N' '-N KUBE-FORWARD'; break ;; esac; done\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "iptables"), []byte(fake), 0o755); err != nil {
		t.Fatalf("fixture write failed: %v", err)
	}
	for _, name := range []string{"ip6tables", "systemctl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("fixture write failed: %v", err)
		}
	}
	// A stand-in for `timeout`, which macOS does not ship; it drops the duration and runs the rest.
	if err := os.WriteFile(filepath.Join(bin, "timeout"), []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatalf("fixture write failed: %v", err)
	}

	for _, s := range vmGroupFirewallCleanupSteps(filepath.Join(root, "conf"),
		filepath.Join(root, "applier"), filepath.Join(root, "units")) {
		runStep(t, s, bin)
	}

	out, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the fake iptables was never called: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		// The FORWARD jump first, so nothing is jumping into a chain being deleted.
		"-D FORWARD -j RUNOS-VMFW",
		// AND the shadow jump. A build that died mid-swap leaves FORWARD pointing at RUNOS-VMFW-N,
		// which is what the applier hooks first and renames only on a clean swap. Without this the
		// jump survived the reset and `-X RUNOS-VMFW-N` was refused, so the chain did too.
		"-D FORWARD -j RUNOS-VMFW-N",
		"-F RUNOS-VMFW-N",
		"-X RUNOS-VMFW-N",
		"-F RUNOS-VMFW",
		"-X RUNOS-VMFW",
		"-F RUNOS-VMFW-vm1abc-IN",
		"-X RUNOS-VMFW-vm1abc-IN",
		"-F RUNOS-VMFW-vm1abc-OUT",
		"-X RUNOS-VMFW-vm1abc-OUT",
		// The `-N` shadow a build that died mid-swap leaves behind.
		"-X RUNOS-VMFW-vm2def-IN-N",
		// BOTH TABLES. The customer firewall lives in mangle since G30-F16, and a node uninstalled
		// after an upgrade can still carry the old filter copies, so neither may be skipped.
		"-t mangle -D FORWARD -j RUNOS-VMFW",
		"-t filter -D FORWARD -j RUNOS-VMFW",
		"-t mangle -X RUNOS-VMFW-vm1abc-IN",
		"-t filter -X RUNOS-VMFW-vm1abc-IN",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("cleanup does not run %q, got:\n%s", want, got)
		}
	}
	// Scoped to what RunOS owns: a cleanup that flushed cilium's or kube-proxy's chains would take
	// the node's own networking with it.
	for _, never := range []string{"CILIUM_FORWARD", "KUBE-FORWARD"} {
		if strings.Contains(got, "-F "+never) || strings.Contains(got, "-X "+never) {
			t.Errorf("cleanup touched %s, which is not RunOS's to touch:\n%s", never, got)
		}
	}
	// FLUSH EVERY CHAIN BEFORE DELETING ANY: `-X` refuses a chain another chain still jumps to, and
	// `iptables -S` lists them in an arbitrary order, so a single flush-then-delete loop would leave
	// whichever per-machine chain it reached before the dispatch chain alive on the node.
	// PER TABLE, not globally. The sweep is `for t in filter mangle`, so the run is
	// filter-flush, filter-delete, mangle-flush, mangle-delete: a global "last flush before first
	// delete" is false BY DESIGN and asserting it would be asserting a bug. What must hold, and
	// what actually protects the chains, is the ordering within each table.
	for _, table := range []string{"filter", "mangle"} {
		lastFlush, firstDelete := -1, -1
		for i, line := range strings.Split(strings.TrimSpace(got), "\n") {
			if strings.HasPrefix(line, "-t "+table+" -F RUNOS-VMFW") {
				lastFlush = i
			}
			if firstDelete < 0 && strings.HasPrefix(line, "-t "+table+" -X RUNOS-VMFW") {
				firstDelete = i
			}
		}
		if lastFlush < 0 || firstDelete < 0 || lastFlush > firstDelete {
			t.Errorf("in table %s every RUNOS-VMFW chain must be flushed (last at %d) before any is deleted (first at %d)",
				table, lastFlush, firstDelete)
		}
	}
}

// Measured on a lab box 2026-08-22. After `clusters reset` uninstalled Kubernetes and rebooted
// the box "back to bare", /var/lib/containerd still held 43 GB on a 63 GB root (80% full). containerd
// itself was inactive, so the image store was pure orphan. The immediate rejoin was then BLOCKED
// by preflight [disk-space], so a box that had been a healthy node ten minutes earlier could not
// be rebuilt. A second host carried 32 GB of the same litter and only escaped because its root is
// 274 GB.
//
// The uninstall already stops containerd before wiping, and already wipes /etc/kubernetes,
// /var/lib/kubelet, /var/lib/etcd and the CNI dirs. The runtime's own data dir was simply missing
// from that list.
func TestRuntimeDataWipeSteps_RemovesTheContainerdImageStore(t *testing.T) {
	steps := runtimeDataWipeSteps()

	joined := strings.Join(steps, "\n")
	if !strings.Contains(joined, "/var/lib/containerd") {
		t.Fatalf("uninstall never wipes /var/lib/containerd, so the next install is blocked by disk space.\nsteps:\n%s", joined)
	}
}

// The wipe must not be able to wedge the uninstall. Every other best-effort step is bounded and
// `|| true`, because a load-bearing step that fails turns into a permanent "partial uninstall" on
// every retry, which is worse than the litter it was cleaning.
func TestRuntimeDataWipeSteps_AreBestEffortAndBounded(t *testing.T) {
	for _, s := range runtimeDataWipeSteps() {
		if !strings.Contains(s, "|| true") {
			t.Errorf("step is not best-effort, a failure would wedge the uninstall: %q", s)
		}
		if !strings.Contains(s, "timeout") {
			t.Errorf("step is unbounded, a busy mount would hang the uninstall: %q", s)
		}
	}
}

// It removes the runtime's data, never its configuration or binaries: a re-install reuses the
// packages and only needs the image store gone.
func TestRuntimeDataWipeSteps_LeavesTheRuntimeInstalled(t *testing.T) {
	joined := strings.Join(runtimeDataWipeSteps(), "\n")
	for _, keep := range []string{"/etc/containerd", "/usr/bin/containerd", "/usr/local/bin/runos"} {
		if strings.Contains(joined, keep) {
			t.Errorf("wipe must not remove %s", keep)
		}
	}
}

// The DNS link guard survived every uninstall, and it does not sit still: it is a
// restart-on-failure unit that carries Wants=wg-quick@wg0.service, so on a node with no cluster
// it fails, restarts, and PULLS wg0 UP on every attempt.
//
// Measured on a lab node 2026-09-01, on the first install after a `nodes delete`:
//
//	15:23:05  runos-clear-link-dns.service: Scheduled restart job, restart counter is at 3
//	15:23:05  Starting wg-quick@wg0.service ... [#] ip link add wg0 type wireguard
//	15:23:06  wg-quick: `wg0' already exists   <- the install, one second later, aborted
//
// So a box RunOS reported as wiped still ran a failing RunOS service in a permanent restart loop,
// and that loop broke the next install. The WireGuard block below it already learned this lesson
// for its own files (goal 23 review, and the reset-failed that followed); these three were simply
// never on the list.
func TestLinkDNSGuardCleanupSteps_RemovesEveryFileTheInstallWrites(t *testing.T) {
	joined := strings.Join(linkDNSGuardCleanupSteps(), "\n")

	// Exactly what nodeward's 12_dns_link_guard.go and 12_dns.go put on the box.
	for _, path := range []string{
		"/usr/local/sbin/runos-clear-link-dns",
		"/etc/systemd/system/runos-clear-link-dns.service",
		"/etc/systemd/system/runos-clear-link-dns.path",
		"/etc/systemd/system/dnsmasq.service.d/wait-for-wireguard.conf",
	} {
		if !strings.Contains(joined, path) {
			t.Errorf("uninstall leaves %s behind, so the box is not wiped.\nsteps:\n%s", path, joined)
		}
	}
}

// Stopping it is not optional and must come BEFORE the files go. A unit whose fragment is deleted
// while it is still loaded keeps its restart timer, so the pull-wg0-up loop outlives the files
// that describe it.
func TestLinkDNSGuardCleanupSteps_StopsTheLoopBeforeDeletingIt(t *testing.T) {
	steps := linkDNSGuardCleanupSteps()
	joined := strings.Join(steps, "\n")

	stop := strings.Index(joined, "systemctl disable --now runos-clear-link-dns.path")
	del := strings.Index(joined, "rm -f /etc/systemd/system/runos-clear-link-dns.service")
	if stop < 0 {
		t.Fatalf("the .path unit is what restarts the service; it must be disabled --now.\nsteps:\n%s", joined)
	}
	if del < 0 || stop > del {
		t.Fatalf("the units must be stopped before their files are removed.\nsteps:\n%s", joined)
	}

	// systemd keeps a failed unit listed after its fragment is gone, which is what left
	// `systemctl is-system-running` answering DEGRADED on a freshly wiped box.
	if !strings.Contains(joined, "reset-failed runos-clear-link-dns") {
		t.Errorf("clear the failed state too, or the wiped box reports DEGRADED.\nsteps:\n%s", joined)
	}
	if !strings.Contains(joined, "daemon-reload") {
		t.Errorf("systemd must be reloaded after the fragments are removed.\nsteps:\n%s", joined)
	}
}

// Same rule as every other best-effort block: bounded and non-fatal, or a wedged systemctl turns
// into a permanent partial uninstall on every retry.
func TestLinkDNSGuardCleanupSteps_AreBestEffortAndBounded(t *testing.T) {
	for _, s := range linkDNSGuardCleanupSteps() {
		if !strings.Contains(s, "|| true") {
			t.Errorf("step is not best-effort, a failure would wedge the uninstall: %q", s)
		}
		if strings.HasPrefix(s, "systemctl") && !strings.Contains(s, "timeout") {
			t.Errorf("systemctl step is unbounded, a wedged systemd would hang the uninstall: %q", s)
		}
	}
}

// It must not take dnsmasq itself with it. dnsmasq is a package RunOS configures, not one it owns,
// and the drop-in is the only part of it the install wrote.
func TestLinkDNSGuardCleanupSteps_RemovesOnlyTheDropInNotDnsmasq(t *testing.T) {
	joined := strings.Join(linkDNSGuardCleanupSteps(), "\n")
	if strings.Contains(joined, "apt-get remove") || strings.Contains(joined, "purge") {
		t.Errorf("the cleanup must not uninstall dnsmasq itself:\n%s", joined)
	}
	if strings.Contains(joined, "rm -rf /etc/dnsmasq") {
		t.Errorf("the cleanup must not remove dnsmasq's own configuration:\n%s", joined)
	}
}
