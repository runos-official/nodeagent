package commons

import (
	"os"
	"path/filepath"
	"testing"
)

// The agent restores nodeward's wg-quick instance unit on start, so a node installed before the
// ordering reset is repaired without a reinstall. These pin the write rules; the content itself
// is pinned against nodeward's copy by a source guard in that repo.

func TestUnitIsWrittenWhenAbsentOrDifferent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "override.conf")

	changed, err := ensureFileContent(path, WgQuickUnit)
	if err != nil || !changed {
		t.Fatalf("absent file must be written: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != WgQuickUnit {
		t.Fatalf("wrote different bytes:\n%s", got)
	}

	// The old, appended-style file a pre-reset install left behind.
	os.WriteFile(path, []byte("[Unit]\nAfter=network-online.target\nWants=network-online.target\n"), 0o644)
	changed, err = ensureFileContent(path, WgQuickUnit)
	if err != nil || !changed {
		t.Fatalf("a differing file must be rewritten: changed=%v err=%v", changed, err)
	}

	changed, err = ensureFileContent(path, WgQuickUnit)
	if err != nil || changed {
		t.Fatalf("a matching file must be left alone: changed=%v err=%v", changed, err)
	}
}

func TestUnitIsNotCreatedWhereTheDirectoryIsMissing(t *testing.T) {
	// No drop-in directory means no wg0 unit yet (mid-install, or not a RunOS node at all).
	path := filepath.Join(t.TempDir(), "wg-quick@wg0.service.d", "override.conf")
	changed, err := ensureFileContent(path, WgQuickUnit)
	if err != nil || changed {
		t.Fatalf("must not create the drop-in directory: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file was created under a directory that did not exist")
	}
}

func TestUnitDoesNotOrderAfterNameResolution(t *testing.T) {
	// nss-lookup.target is the cycle; the unit is the stock template without it.
	for _, l := range splitLines(WgQuickUnit) {
		if len(l) > 0 && l[0] == '#' {
			continue
		}
		if contains(l, "nss-lookup") {
			t.Fatalf("the wg0 unit must not depend on nss-lookup.target: %q", l)
		}
	}
	for _, want := range []string{"\nAfter=network-online.target\n", "ExecStart=/bin/sh -c 'ip link show wg0 >/dev/null 2>&1 || exec /usr/bin/wg-quick up wg0'\n", "WantedBy=multi-user.target\n"} {
		if !contains(WgQuickUnit, want) {
			t.Fatalf("unit lacks %q", want)
		}
	}
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// G28-F2. A node installed before this fix ends with wg-quick@wg0 in the failed state while wg0
// is up, so `systemctl is-system-running` reports degraded until the first reboot. Nothing
// re-installs a node already in the fleet, so the agent repairs it.

func TestTheUnitExecStartSurvivesAnExistingWg0(t *testing.T) {
	if !contains(WgQuickUnit, "ip link show wg0 >/dev/null 2>&1 || exec /usr/bin/wg-quick up wg0") {
		t.Fatal("ExecStart must be a no-op when wg0 already exists, or every Wants= pull fails the unit")
	}
}

func TestAFailedUnitIsResetAndStarted(t *testing.T) {
	var calls [][]string
	orig := systemctlRun
	defer func() { systemctlRun = orig }()
	systemctlRun = func(args ...string) (string, error) {
		calls = append(calls, args)
		if args[0] == "is-failed" {
			return "failed\n", nil
		}
		return "", nil
	}

	clearFailedWg0Unit()

	if len(calls) != 4 {
		t.Fatalf("want is-failed, reset-failed, daemon-reload, start; got %v", calls)
	}
	for i, want := range []string{"is-failed", "reset-failed", "daemon-reload", "start"} {
		if calls[i][0] != want {
			t.Fatalf("call %d: want %s, got %v", i, want, calls[i])
		}
	}
	// The daemon-reload must sit BETWEEN the clear and the start, or the start can execute the
	// stale unguarded unit systemd still holds after an earlier reload failed, and the node stays
	// degraded through every agent start until its next reboot.
	for _, i := range []int{0, 1, 3} {
		if calls[i][1] != "wg-quick@wg0" {
			t.Fatalf("call %d addressed %q, not the wg0 unit", i, calls[i][1])
		}
	}
	if len(calls[2]) != 1 {
		t.Fatalf("daemon-reload takes no unit argument, got %v", calls[2])
	}
}

func TestAHealthyUnitIsLeftAlone(t *testing.T) {
	var calls [][]string
	orig := systemctlRun
	defer func() { systemctlRun = orig }()
	systemctlRun = func(args ...string) (string, error) {
		calls = append(calls, args)
		// systemctl is-failed exits non-zero and prints the real state when the unit is fine.
		return "active\n", errNotFailed{}
	}

	clearFailedWg0Unit()

	if len(calls) != 1 {
		t.Fatalf("a healthy unit must cost exactly one read, got %v", calls)
	}
}

type errNotFailed struct{}

func (errNotFailed) Error() string { return "exit status 1" }
