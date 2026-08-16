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

func TestVmGroupBridgeCleanupSteps_ReloadsNetworkd(t *testing.T) {
	// Removing the units without a reload leaves networkd holding the old configuration, so the
	// bridge comes back the next time anything touches the interface.
	joined := strings.Join(vmGroupBridgeCleanupSteps("/etc/systemd/network"), "\n")
	if !strings.Contains(joined, "networkctl reload") {
		t.Errorf("expected a networkctl reload, got:\n%s", joined)
	}
}
