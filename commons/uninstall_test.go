package commons

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// R4, measured on ftb1 2026-08-16: after `runos uninstall` (and after the control-plane-driven
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
