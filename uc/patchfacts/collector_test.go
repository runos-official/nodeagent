package patchfacts

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Criterion 1. Every field of the fact contract, read from captured output of a
// running node, with a collected-at time.
func TestCollectEveryFieldFromANode(t *testing.T) {
	node := capturedNode(t)
	rec, err := node.collector().Collect(Options{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if rec.CollectedAt != node.now {
		t.Errorf("CollectedAt = %v, want %v", rec.CollectedAt, node.now)
	}
	if rec.RunningKernel != "6.8.0-147-generic" {
		t.Errorf("RunningKernel = %q", rec.RunningKernel)
	}
	if want := []string{"6.8.0-147-generic"}; !reflect.DeepEqual(rec.InstalledKernels, want) {
		t.Errorf("InstalledKernels = %v, want %v", rec.InstalledKernels, want)
	}
	if rec.NewestInstalledKernel != "6.8.0-147-generic" {
		t.Errorf("NewestInstalledKernel = %q", rec.NewestInstalledKernel)
	}
	if rec.RebootRequired {
		t.Error("RebootRequired = true, want false: the node carries no reboot-required flag")
	}
	if rec.RebootRequiredSince != nil {
		t.Errorf("RebootRequiredSince = %v, want nil", rec.RebootRequiredSince)
	}
	if len(rec.RebootTriggerPackages) != 0 {
		t.Errorf("RebootTriggerPackages = %v, want empty", rec.RebootTriggerPackages)
	}
	if rec.SecurityUpdateCount == nil || *rec.SecurityUpdateCount != 25 {
		t.Errorf("SecurityUpdateCount = %v, want 25", rec.SecurityUpdateCount)
	}
	if !reflect.DeepEqual(rec.SecurityUpdatePackages, capturedSecurityNames) {
		t.Errorf("SecurityUpdatePackages =\n%v\nwant\n%v", rec.SecurityUpdatePackages, capturedSecurityNames)
	}
	if !rec.SecurityUpdatesStale {
		t.Error("SecurityUpdatesStale = false, want true: the metadata is about six days old")
	}
	if rec.PackageMetadataAgeSeconds == nil || *rec.PackageMetadataAgeSeconds != 507739 {
		t.Errorf("PackageMetadataAgeSeconds = %v, want 507739", rec.PackageMetadataAgeSeconds)
	}
	if rec.PackageMetadataSource != pathUpdateSuccessStamp {
		t.Errorf("PackageMetadataSource = %q, want %q", rec.PackageMetadataSource, pathUpdateSuccessStamp)
	}
	wantRun := time.Date(2026, 11, 2, 21, 50, 16, 0, time.Local).UTC()
	if rec.LastPackageRunAt == nil || !rec.LastPackageRunAt.Equal(wantRun) {
		t.Errorf("LastPackageRunAt = %v, want %v", rec.LastPackageRunAt, wantRun)
	}
	wantBoot := time.Unix(1793700929, 0).UTC()
	if rec.BootTime == nil || !rec.BootTime.Equal(wantBoot) {
		t.Errorf("BootTime = %v, want %v", rec.BootTime, wantBoot)
	}
	if rec.AutomaticUpdateUnitState != UnitStateMasked {
		t.Errorf("AutomaticUpdateUnitState = %q, want %q", rec.AutomaticUpdateUnitState, UnitStateMasked)
	}
	wantHeld := []HeldPackage{
		{Name: "kubeadm", Version: "1.36.5-1.1"},
		{Name: "kubectl", Version: "1.36.5-1.1"},
		{Name: "kubelet", Version: "1.36.5-1.1"},
	}
	if !reflect.DeepEqual(rec.HeldPackages, wantHeld) {
		t.Errorf("HeldPackages = %v, want %v", rec.HeldPackages, wantHeld)
	}
	if len(rec.FieldErrors) != 0 {
		t.Errorf("FieldErrors = %v, want none: every field was readable", rec.FieldErrors)
	}
}

// Criterion 2. The waiting security update count excludes every held package.
// The hold list is Contract D's, written in apt-mark showhold's own format,
// because no node holds that whole set yet. haproxy is the proof: it IS a
// security update on the captured node, and holding it must take it out of both
// the count and the names.
func TestHeldPackageWithANewerSecurityCandidateIsNotCounted(t *testing.T) {
	node := capturedNode(t)
	node.showholdOut = fixture(t, "contractd_apt_mark_showhold.txt")
	node.dpkgVersionsOut = fixture(t, "dpkg_query_held_versions_six_packages.txt")

	rec, err := node.collector().Collect(Options{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if rec.SecurityUpdateCount == nil || *rec.SecurityUpdateCount != 24 {
		t.Fatalf("SecurityUpdateCount = %v, want 24 (25 less the held haproxy)", rec.SecurityUpdateCount)
	}
	for _, name := range rec.SecurityUpdatePackages {
		if name == "haproxy" {
			t.Error("SecurityUpdatePackages still names haproxy, which is held")
		}
		if name == "dnsmasq" {
			t.Error("SecurityUpdatePackages names dnsmasq, which is held and is not a security update either")
		}
	}
	if len(rec.HeldPackages) != 11 {
		t.Errorf("HeldPackages has %d entries, want 11", len(rec.HeldPackages))
	}
	// A held package dpkg did not answer for keeps its place in the list with
	// an empty version, so the record never drops a hold it was told about.
	byName := map[string]string{}
	for _, h := range rec.HeldPackages {
		byName[h.Name] = h.Version
	}
	if byName["containerd"] != "2.2.1-0ubuntu1~24.04.4" {
		t.Errorf("containerd held at %q", byName["containerd"])
	}
	if v, ok := byName["etcd-client"]; !ok || v != "" {
		t.Errorf("etcd-client = %q, present %v; want present with an empty version", v, ok)
	}
}

// Criterion 3. The record carries the age of the metadata the count came from,
// and a count from metadata past the bound is reported stale, never as zero.
func TestMetadataAgeAndStaleness(t *testing.T) {
	const stamp = 1794163948

	t.Run("fresh metadata gives a count and no stale flag", func(t *testing.T) {
		node := capturedNode(t)
		node.modTimes[pathUpdateSuccessStamp] = node.now.Add(-time.Hour)
		rec, err := node.collector().Collect(Options{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if rec.SecurityUpdateCount == nil || *rec.SecurityUpdateCount != 25 {
			t.Errorf("SecurityUpdateCount = %v, want 25", rec.SecurityUpdateCount)
		}
		if rec.SecurityUpdatesStale {
			t.Error("SecurityUpdatesStale = true for metadata one hour old")
		}
		if rec.PackageMetadataAgeSeconds == nil || *rec.PackageMetadataAgeSeconds != 3600 {
			t.Errorf("PackageMetadataAgeSeconds = %v, want 3600", rec.PackageMetadataAgeSeconds)
		}
	})

	t.Run("metadata past the bound gives the count AND the stale flag", func(t *testing.T) {
		node := capturedNode(t)
		node.modTimes[pathUpdateSuccessStamp] = node.now.Add(-MetadataStaleAfter - time.Second)
		rec, err := node.collector().Collect(Options{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if rec.SecurityUpdateCount == nil || *rec.SecurityUpdateCount != 25 {
			t.Errorf("SecurityUpdateCount = %v, want 25: a stale count is still reported", rec.SecurityUpdateCount)
		}
		if !rec.SecurityUpdatesStale {
			t.Error("SecurityUpdatesStale = false for metadata past the bound")
		}
	})

	t.Run("metadata that cannot be read gives the stale flag, no count and a field error", func(t *testing.T) {
		node := capturedNode(t)
		delete(node.modTimes, pathUpdateSuccessStamp)
		// The index directory cannot be read either, so no fallback exists.
		rec, err := node.collector().Collect(Options{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if rec.SecurityUpdateCount != nil {
			t.Errorf("SecurityUpdateCount = %v, want nil: a zero here would read as Up to date", *rec.SecurityUpdateCount)
		}
		if !rec.SecurityUpdatesStale {
			t.Error("SecurityUpdatesStale = false although the metadata age is unknown")
		}
		if rec.PackageMetadataAgeSeconds != nil {
			t.Errorf("PackageMetadataAgeSeconds = %v, want nil", *rec.PackageMetadataAgeSeconds)
		}
		if rec.FieldErrors["packageMetadataAgeSeconds"] == "" {
			t.Error("FieldErrors has no entry for packageMetadataAgeSeconds")
		}
		if rec.FieldErrors["securityUpdateCount"] == "" {
			t.Error("FieldErrors has no entry for securityUpdateCount")
		}
		if node.ran("apt-get -s") {
			t.Error("the collector simulated an upgrade although it had no metadata age to report the count with")
		}
	})

	t.Run("the index files are the fallback when the stamp is missing", func(t *testing.T) {
		node := capturedNode(t)
		delete(node.modTimes, pathUpdateSuccessStamp)
		newest := time.Unix(stamp, 0).UTC().Add(-2 * time.Hour)
		node.dirs = map[string][]fakeDirEntry{pathAptLists: {
			{name: "lock", modTime: time.Unix(stamp, 0).UTC()},
			{name: "partial", dir: true, modTime: time.Unix(stamp, 0).UTC()},
			{name: "security.ubuntu.com_ubuntu_dists_noble-security_InRelease", modTime: newest},
			{name: "archive.ubuntu.com_ubuntu_dists_noble_InRelease", modTime: newest.Add(-500 * time.Hour)},
		}}
		rec, err := node.collector().Collect(Options{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if rec.PackageMetadataSource != pathAptLists+"/security.ubuntu.com_ubuntu_dists_noble-security_InRelease" {
			t.Errorf("PackageMetadataSource = %q", rec.PackageMetadataSource)
		}
		if rec.PackageMetadataAgeSeconds == nil || *rec.PackageMetadataAgeSeconds != 7200 {
			t.Errorf("PackageMetadataAgeSeconds = %v, want 7200", rec.PackageMetadataAgeSeconds)
		}
		if rec.SecurityUpdatesStale {
			t.Error("SecurityUpdatesStale = true for an index file two hours old")
		}
	})
}

// Criterion 4. The refresh runs on the slow pass only, and the collector gives
// up rather than waiting out a package manager lock.
func TestRefreshRunsOnTheSlowPassOnlyAndStandsDownOnALock(t *testing.T) {
	t.Run("a slow pass with no lock refreshes", func(t *testing.T) {
		node := capturedNode(t)
		rec, err := node.collector().Collect(Options{SlowPass: true})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if node.refreshCalls != 1 {
			t.Errorf("the refresh ran %d times, want 1", node.refreshCalls)
		}
		if rec.FieldErrors["packageMetadataAgeSeconds"] != "" {
			t.Errorf("unexpected field error: %q", rec.FieldErrors["packageMetadataAgeSeconds"])
		}
	})

	t.Run("a slow pass with a held lock runs no refresh and reports the metadata as it stands", func(t *testing.T) {
		node := capturedNode(t)
		node.lockIsHeld = true
		node.lockHolder = "/var/lib/dpkg/lock-frontend"
		rec, err := node.collector().Collect(Options{SlowPass: true})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if node.refreshCalls != 0 {
			t.Errorf("the refresh ran %d times although a lock was held", node.refreshCalls)
		}
		// The previous count is still reported, with the age of the metadata it
		// came from, so the pass needs no state carried from the last one.
		if rec.SecurityUpdateCount == nil || *rec.SecurityUpdateCount != 25 {
			t.Errorf("SecurityUpdateCount = %v, want 25", rec.SecurityUpdateCount)
		}
		if rec.PackageMetadataAgeSeconds == nil || *rec.PackageMetadataAgeSeconds != 507739 {
			t.Errorf("PackageMetadataAgeSeconds = %v, want 507739", rec.PackageMetadataAgeSeconds)
		}
		if !strings.Contains(rec.FieldErrors["packageMetadataAgeSeconds"], "/var/lib/dpkg/lock-frontend") {
			t.Errorf("the field error does not name the held lock: %q", rec.FieldErrors["packageMetadataAgeSeconds"])
		}
	})

	t.Run("a non-slow pass never refreshes", func(t *testing.T) {
		node := capturedNode(t)
		if _, err := node.collector().Collect(Options{}); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if node.refreshCalls != 0 {
			t.Errorf("the refresh ran %d times on a pass that is not the slow one", node.refreshCalls)
		}
		if node.ran("update") {
			t.Error("the collector ran an apt update on a pass that is not the slow one")
		}
	})

	t.Run("the lock is probed before the refresh, never after it", func(t *testing.T) {
		node := capturedNode(t)
		node.lockErr = errors.New("permission denied")
		rec, err := node.collector().Collect(Options{SlowPass: true})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if node.refreshCalls != 0 {
			t.Error("the refresh ran although the lock probe failed")
		}
		if !strings.Contains(rec.FieldErrors["packageMetadataAgeSeconds"], "permission denied") {
			t.Errorf("the field error does not carry the probe failure: %q", rec.FieldErrors["packageMetadataAgeSeconds"])
		}
	})
}

// Criterion 5. The automatic unit state is one of masked, enabled outside RunOS
// or unknown, and there is no managed member.
func TestAutomaticUpdateUnitState(t *testing.T) {
	enabled := func(id string) string {
		return fmt.Sprintf("Id=%s\nLoadState=loaded\nActiveState=active\nUnitFileState=enabled\n", id)
	}
	masked := func(id string) string {
		return fmt.Sprintf("Id=%s\nLoadState=masked\nActiveState=inactive\nUnitFileState=masked\n", id)
	}

	t.Run("every unit masked gives masked", func(t *testing.T) {
		node := capturedNode(t)
		rec, err := node.collector().Collect(Options{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if rec.AutomaticUpdateUnitState != UnitStateMasked {
			t.Errorf("state = %q, want %q", rec.AutomaticUpdateUnitState, UnitStateMasked)
		}
	})

	t.Run("a unit enabled outside RunOS gives enabled_outside_runos", func(t *testing.T) {
		node := capturedNode(t)
		units := maskedUnits(t)
		units["apt-daily-upgrade.timer"] = enabled("apt-daily-upgrade.timer")
		node.unitOut = units
		rec, err := node.collector().Collect(Options{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if rec.AutomaticUpdateUnitState != UnitStateEnabledOutsideRunOS {
			t.Errorf("state = %q, want %q", rec.AutomaticUpdateUnitState, UnitStateEnabledOutsideRunOS)
		}
	})

	t.Run("an unreadable unit gives unknown", func(t *testing.T) {
		node := capturedNode(t)
		node.unitErr = errors.New("Failed to connect to bus")
		rec, err := node.collector().Collect(Options{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if rec.AutomaticUpdateUnitState != UnitStateUnknown {
			t.Errorf("state = %q, want %q", rec.AutomaticUpdateUnitState, UnitStateUnknown)
		}
		if rec.FieldErrors["automaticUpdateUnitState"] == "" {
			t.Error("FieldErrors has no entry for automaticUpdateUnitState")
		}
	})

	t.Run("an enabled timer with the periodic settings at zero is not enabled outside RunOS", func(t *testing.T) {
		node := capturedNode(t)
		units := map[string]string{}
		for _, u := range automaticUpdateUnits {
			units[u] = enabled(u)
		}
		node.unitOut = units
		node.aptConfigOut = "APT::Periodic \"\";\nAPT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"0\";\n"
		rec, err := node.collector().Collect(Options{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if rec.AutomaticUpdateUnitState != UnitStateUnknown {
			t.Errorf("state = %q, want %q: that node installs nothing, so the plain reading would be wrong",
				rec.AutomaticUpdateUnitState, UnitStateUnknown)
		}
	})

	t.Run("the enum has no managed member", func(t *testing.T) {
		for _, state := range []string{UnitStateMasked, UnitStateEnabledOutsideRunOS, UnitStateUnknown} {
			if strings.Contains(state, "manage") {
				t.Errorf("the enum carries a managed state: %q", state)
			}
		}
		// The wire form is snake_case, because three other repositories of this
		// objective validate and render these exact words.
		if UnitStateEnabledOutsideRunOS != "enabled_outside_runos" {
			t.Errorf("the middle member is spelled %q, want enabled_outside_runos", UnitStateEnabledOutsideRunOS)
		}
	})

	t.Run("a masked and an enabled unit read the same way on a real node", func(t *testing.T) {
		// The value vocabulary is captured, not invented: these are the states
		// systemctl printed on a running node for units that are not masked.
		samples := unitsFromBlocks(t, fixture(t, "systemctl_show_not_masked_units.txt"))
		st, err := parseUnitState(samples["logrotate.timer"])
		if err != nil {
			t.Fatalf("parseUnitState: %v", err)
		}
		if st.masked() || !st.couldRun() {
			t.Errorf("logrotate.timer read as masked=%v couldRun=%v, want false and true", st.masked(), st.couldRun())
		}
		_ = masked
	})
}

// Criterion 6. The reboot-required flag carries its trigger package list.
func TestRebootRequiredAndItsTriggers(t *testing.T) {
	const flagged = 1794160000

	t.Run("a pending reboot with triggers", func(t *testing.T) {
		node := capturedNode(t)
		node.modTimes[pathRebootRequired] = time.Unix(flagged, 0).UTC()
		node.files[pathRebootRequired] = "*** System restart required ***\n"
		node.files[pathRebootRequiredPkg] = "linux-image-6.8.0-148-generic\nlibc6\n"
		rec, err := node.collector().Collect(Options{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if !rec.RebootRequired {
			t.Error("RebootRequired = false although the flag file exists")
		}
		want := []string{"linux-image-6.8.0-148-generic", "libc6"}
		if !reflect.DeepEqual(rec.RebootTriggerPackages, want) {
			t.Errorf("RebootTriggerPackages = %v, want %v", rec.RebootTriggerPackages, want)
		}
		if rec.RebootRequiredSince == nil || !rec.RebootRequiredSince.Equal(time.Unix(flagged, 0).UTC()) {
			t.Errorf("RebootRequiredSince = %v, want %v", rec.RebootRequiredSince, time.Unix(flagged, 0).UTC())
		}
		// The onset is the flag's own time, not the boot time, so the server's
		// reboot-pending age is minutes and not days.
		if rec.BootTime != nil && !rec.RebootRequiredSince.After(*rec.BootTime) {
			t.Error("RebootRequiredSince is not after BootTime")
		}
	})

	t.Run("a pending reboot with no readable trigger list", func(t *testing.T) {
		node := capturedNode(t)
		node.modTimes[pathRebootRequired] = time.Unix(flagged, 0).UTC()
		rec, err := node.collector().Collect(Options{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if !rec.RebootRequired {
			t.Error("RebootRequired = false: an unreadable trigger list must not drop the reboot fact")
		}
		if len(rec.RebootTriggerPackages) != 0 {
			t.Errorf("RebootTriggerPackages = %v, want empty", rec.RebootTriggerPackages)
		}
		if rec.FieldErrors["rebootTriggerPackages"] == "" {
			t.Error("FieldErrors has no entry for rebootTriggerPackages")
		}
	})

	t.Run("no pending reboot", func(t *testing.T) {
		node := capturedNode(t)
		rec, err := node.collector().Collect(Options{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if rec.RebootRequired || rec.RebootRequiredSince != nil {
			t.Errorf("RebootRequired = %v, since = %v; want false and nil", rec.RebootRequired, rec.RebootRequiredSince)
		}
		if _, noted := rec.FieldErrors["rebootRequired"]; noted {
			t.Error("an absent flag file was recorded as a read error; it is a real answer")
		}
	})
}

// Criterion 7. Every command the collector can run is a row of one table, and
// no row changes the node except the one metadata refresh.
func TestCommandTableChangesNothing(t *testing.T) {
	writers := 0
	for _, row := range commandTable {
		argv := append([]string{row.name}, row.args...)
		if bad, why := mutatingArgv(argv); bad {
			t.Errorf("command row %q (%v) %s", row.key, argv, why)
		}
		if row.writes {
			writers++
			if row.key != cmdAptRefresh {
				t.Errorf("command row %q claims to write; only %q may", row.key, cmdAptRefresh)
			}
		}
	}
	if writers != 1 {
		t.Errorf("%d rows write, want exactly 1 (the package metadata refresh)", writers)
	}

	// The check is not vacuous: it catches a row that installs.
	for _, argv := range [][]string{
		{"apt-get", "install", "-y", "haproxy"},
		{"apt-get", "upgrade"},
		{"apt-mark", "unhold", "kubelet"},
		{"dpkg", "--purge", "containerd"},
		{"apt-get", "-s", "--ignore-hold", "upgrade"},
	} {
		if bad, _ := mutatingArgv(argv); !bad {
			t.Errorf("mutatingArgv let %v through", argv)
		}
	}
}

// mutatingArgv reports whether an argument vector could change the packages on
// a node.
func mutatingArgv(argv []string) (bool, string) {
	forbidden := []string{
		"install", "reinstall", "remove", "purge", "autoremove", "autoclean",
		"clean", "hold", "unhold", "dist-upgrade", "full-upgrade",
		"--ignore-hold", "--force-yes", "--allow-downgrades", "-y", "--yes",
	}
	simulated := false
	for _, arg := range argv {
		if arg == "-s" || arg == "--simulate" || arg == "--just-print" || arg == "--dry-run" {
			simulated = true
		}
	}
	for _, arg := range argv {
		// A verb and its flag spelling are the same thing to the package
		// manager: purge, --purge and -purge all remove a package.
		bare := strings.TrimLeft(arg, "-")
		for _, f := range forbidden {
			stripped := strings.TrimLeft(f, "-")
			if bare == stripped || strings.HasPrefix(bare, stripped+"=") {
				return true, fmt.Sprintf("carries the mutating argument %q", arg)
			}
		}
		if arg == "upgrade" && !simulated {
			return true, "upgrades packages without simulating"
		}
	}
	return false, ""
}

// Criterion 7, the second half: the collector can reach the node only through
// the table.
func TestCollectorRunsNothingOutsideTheTable(t *testing.T) {
	node := capturedNode(t)
	c := node.collector()
	if _, err := c.exec("apt-get-install"); err == nil {
		t.Error("exec accepted a key that is not a table row")
	}
	if _, err := c.exec(cmdUname, "extra"); err == nil {
		t.Error("exec appended a name to a row that takes none")
	}

	if _, err := c.Collect(Options{SlowPass: true}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	allowed := map[string]bool{}
	for _, row := range commandTable {
		allowed[row.name] = true
	}
	for _, call := range node.calls {
		program := strings.Fields(call)[0]
		if !allowed[program] {
			t.Errorf("the collector ran %q, which no table row names", program)
		}
		if bad, why := mutatingArgv(strings.Fields(call)); bad {
			t.Errorf("the collector ran %q, which %s", call, why)
		}
	}
}

// safeName keeps a surprising name out of a command's options.
func TestSafeName(t *testing.T) {
	for _, ok := range []string{"haproxy", "libglib2.0-0t64", "g++", "linux-image-6.8.0-147-generic", "kubelet:amd64"} {
		if !safeName(ok) {
			t.Errorf("safeName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "-o", "--force-yes", "a b", "a;b", "a$(b)", "a\nb"} {
		if safeName(bad) {
			t.Errorf("safeName(%q) = true", bad)
		}
	}
}

// The shared partial-record fixture of a DEGRADED node: unreadable package
// metadata with its field error, a null count, a null reboot onset, and every
// other field readable. The store, the carrier and the read endpoint of this
// objective use the same fixture, and no hop may turn an unknown observation
// into a zero or a false, or refuse the readable fields.
func TestDegradedNodeFixture(t *testing.T) {
	node := capturedNode(t)
	delete(node.modTimes, pathUpdateSuccessStamp)
	node.modTimes[pathRebootRequired] = time.Time{}
	node.files[pathRebootRequired] = "*** System restart required ***\n"
	node.files[pathRebootRequiredPkg] = "linux-image-6.8.0-148-generic\n"

	rec, err := node.collector().Collect(Options{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if rec.SecurityUpdateCount != nil {
		t.Errorf("SecurityUpdateCount = %v, want nil", *rec.SecurityUpdateCount)
	}
	if rec.RebootRequiredSince != nil {
		t.Errorf("RebootRequiredSince = %v, want nil", rec.RebootRequiredSince)
	}
	if !rec.RebootRequired {
		t.Error("RebootRequired = false; the readable fields must survive the unreadable ones")
	}
	if rec.RunningKernel == "" || rec.NewestInstalledKernel == "" || len(rec.HeldPackages) == 0 {
		t.Error("a readable field was dropped because another field could not be read")
	}
	for _, field := range []string{"securityUpdateCount", "packageMetadataAgeSeconds", "rebootRequiredSince"} {
		if rec.FieldErrors[field] == "" {
			t.Errorf("FieldErrors has no entry for %s", field)
		}
	}

	got, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	const golden = "testdata/degraded_node_record.json"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Errorf("the shared degraded-node record changed.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// Collect never fails the whole record because one command failed.
func TestCollectSurvivesEveryCommandFailing(t *testing.T) {
	node := &fakeNode{
		unameErr:       errors.New("no uname"),
		dpkgKernelsErr: errors.New("no dpkg"),
		showholdErr:    errors.New("no apt-mark"),
		simulateErr:    errors.New("no apt-get"),
		aptConfigErr:   errors.New("no apt-config"),
		unitErr:        errors.New("no systemctl"),
		now:            time.Unix(1794163948, 0).UTC(),
	}
	rec, err := node.collector().Collect(Options{})
	if err != nil {
		t.Fatalf("Collect returned an error instead of a record with field errors: %v", err)
	}
	if rec.CollectedAt.IsZero() {
		t.Error("CollectedAt is zero")
	}
	for _, field := range []string{
		"runningKernel", "bootTime", "installedKernels", "heldPackages",
		"automaticUpdateUnitState", "packageMetadataAgeSeconds",
		"securityUpdateCount", "lastPackageRunAt",
	} {
		if rec.FieldErrors[field] == "" {
			t.Errorf("FieldErrors has no entry for %s", field)
		}
	}
	if rec.AutomaticUpdateUnitState != UnitStateUnknown {
		t.Errorf("AutomaticUpdateUnitState = %q, want %q", rec.AutomaticUpdateUnitState, UnitStateUnknown)
	}
	if rec.SecurityUpdateCount != nil {
		t.Error("SecurityUpdateCount is set although nothing could be read")
	}
}

// The last successful package run comes from the apt history, and a run that
// recorded an error does not count even though apt still wrote its end time.
func TestLastSuccessfulPackageRun(t *testing.T) {
	t.Run("the newest finished run of the live log", func(t *testing.T) {
		got, ok := lastSuccessfulRun(fixture(t, "apt_history.log"))
		if !ok {
			t.Fatal("no run found in the captured history")
		}
		want := time.Date(2026, 11, 2, 21, 50, 16, 0, time.Local).UTC()
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("a failed run does not count", func(t *testing.T) {
		// apt writes End-Date even for a run that failed, and records the
		// failure as an Error line just before it. Format read from apt's own
		// dpkgpm.cc.
		history := "Start-Date: 2026-11-01  10:00:00\n" +
			"Commandline: apt-get upgrade\n" +
			"Upgrade: haproxy:amd64 (2.8.5-1ubuntu3, 2.8.30-0ubuntu0.24.04.1)\n" +
			"End-Date: 2026-11-01  10:00:05\n" +
			"\n" +
			"Start-Date: 2026-11-02  11:00:00\n" +
			"Commandline: apt-get upgrade\n" +
			"Error: Sub-process /usr/bin/dpkg returned an error code (1)\n" +
			"End-Date: 2026-11-02  11:00:09\n"
		got, ok := lastSuccessfulRun(history)
		if !ok {
			t.Fatal("no run found")
		}
		want := time.Date(2026, 11, 1, 10, 0, 5, 0, time.Local).UTC()
		if !got.Equal(want) {
			t.Errorf("got %v, want %v: the failed run must not win", got, want)
		}
	})

	t.Run("the rotated log is read only when the live log holds no finished run", func(t *testing.T) {
		rotated := "Start-Date: 2026-10-26  18:37:00\nCommandline: apt-get install -y curl\nEnd-Date: 2026-10-26  18:37:04\n"
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write([]byte(rotated)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}

		node := capturedNode(t)
		node.files[pathAptHistory] = "Start-Date: 2026-11-02  21:50:15\nCommandline: apt-get install -y cri-tools\n"
		node.files[pathAptHistoryRotated] = buf.String()
		rec, err := node.collector().Collect(Options{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		want := time.Date(2026, 10, 26, 18, 37, 4, 0, time.Local).UTC()
		if rec.LastPackageRunAt == nil || !rec.LastPackageRunAt.Equal(want) {
			t.Errorf("LastPackageRunAt = %v, want %v", rec.LastPackageRunAt, want)
		}

		// With a finished run in the live log, the rotated one is not read.
		node2 := capturedNode(t)
		node2.files[pathAptHistoryRotated] = "unreadable"
		if _, err := node2.collector().Collect(Options{}); err != nil {
			t.Fatalf("Collect: %v", err)
		}
	})
}

// The installed kernel list keeps only real kernel images that dpkg reports as
// installed, and orders them newest first.
func TestInstalledKernels(t *testing.T) {
	// Captured on a running node: a meta package and a not-installed row with an
	// EMPTY version both appear, so both must be filtered out.
	got := parseInstalledKernels(fixture(t, "dpkg_query_kernels_generic.txt"))
	if want := []string{"6.8.0-147-generic"}; !reflect.DeepEqual(got, want) {
		t.Errorf("kernels of the first captured node = %v, want %v", got, want)
	}
	// Captured on a second running node: the meta package is linux-image-virtual.
	got = parseInstalledKernels(fixture(t, "dpkg_query_kernels_virtual.txt"))
	if want := []string{"6.8.0-142-generic"}; !reflect.DeepEqual(got, want) {
		t.Errorf("kernels of the second captured node = %v, want %v", got, want)
	}
	// Two kernels order by the Debian comparison, not as strings.
	multi := "linux-image-6.8.0-60-generic 6.8.0-60.63 installed\n" +
		"linux-image-6.8.0-147-generic 6.8.0-147.147 installed\n" +
		"linux-image-generic 6.8.0-147.147 installed\n"
	got = parseInstalledKernels(multi)
	if want := []string{"6.8.0-147-generic", "6.8.0-60-generic"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ordered kernels = %v, want %v", got, want)
	}
}

// The Debian version comparison is the one dpkg implements.
func TestCompareDebianVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0},
		{"1.0", "1.0-1", -1},
		{"1.0-1", "1.0-2", -1},
		{"1.0", "1:1.0", -1},
		{"1.0~rc1", "1.0", -1},
		{"1.0~~", "1.0~", -1},
		{"6.8.0-60.63", "6.8.0-147.147", -1},
		{"2.8.5-1ubuntu3", "2.8.30-0ubuntu0.24.04.1", -1},
		{"255.4-1ubuntu8.17", "255.4-1ubuntu8.18", -1},
		{"5.6.1+really5.4.5-1ubuntu0.3", "5.6.1+really5.4.5-1ubuntu0.4", -1},
		{"1:2.43.0-1ubuntu7.3", "1:2.43.0-1ubuntu7.4", -1},
		{"2026c-0ubuntu0.24.04.1", "2026e-0ubuntu0.24.04.1", -1},
		{"0.72-0ubuntu0.24.04.1", "2026.10.12-0ubuntu0.24.04.1", -1},
		{"4.0.1really4.0.1-0ubuntu0.24.04.5", "4.0.1really4.0.1-0ubuntu0.24.04.9", -1},
		{"1.9.33-0ubuntu1~24.04.1ubuntu1", "1.9.34-0ubuntu1~24.04.1", -1},
		{"37.2ubuntu~24.04.1", "38ubuntu0~24.04", -1},
		{"2.76+ubuntu24.04.1", "2.77.1+ubuntu24.04", -1},
		{"13ubuntu10.4", "13ubuntu10.5", -1},
		{"1.0.0", "1.0", 1},
		{"01.0", "1.0", 0},
	}
	for _, tc := range cases {
		if got := sign(compareDebianVersions(tc.a, tc.b)); got != tc.want {
			t.Errorf("compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := sign(compareDebianVersions(tc.b, tc.a)); got != -tc.want {
			t.Errorf("compare(%q, %q) = %d, want %d", tc.b, tc.a, got, -tc.want)
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// A security update is named by the origin and archive of the index's own
// Release file, and a masked newer security version counts.
func TestSecurityClassification(t *testing.T) {
	indexes := parsePolicyIndexes(fixture(t, "apt_cache_policy_indexes.txt"))
	if len(indexes) != 18 {
		t.Fatalf("parsed %d package indexes, want 18", len(indexes))
	}
	security := indexes["http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages"]
	if security.origin != "Ubuntu" || security.archive != "noble-security" {
		t.Errorf("the security index read as origin %q archive %q", security.origin, security.archive)
	}

	versions := parsePolicyPackages(fixture(t, "apt_cache_policy_packages.txt"))
	// haproxy's candidate comes from noble-updates, and it is a security
	// update only because a version newer than the installed one sits in
	// noble-security. The candidate's own pockets would miss it, which is the
	// measured gap against the distribution's own counter.
	if !isSecurityUpgrade(versions["haproxy"], "2.8.5-1ubuntu3", indexes, "noble") {
		t.Error("haproxy did not read as a security update")
	}
	// A package whose only newer versions sit outside a security pocket is not
	// a security update.
	if isSecurityUpgrade(versions["snapd"], "2.76+ubuntu24.04.1", indexes, "noble") {
		t.Error("snapd read as a security update")
	}
	// The codename matters: the pocket of another release is not this node's.
	if isSecurityUpgrade(versions["haproxy"], "2.8.5-1ubuntu3", indexes, "jammy") {
		t.Error("a noble node counted a jammy security pocket")
	}
}

// The fcntl probe is the one apt's own locks answer. flock would not see them.
func TestPackageLockProbe(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/lock"
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	held, err := fcntlLockHeld(path)
	if err != nil {
		t.Fatalf("probe an unlocked file: %v", err)
	}
	if held {
		t.Error("an unlocked file read as held")
	}

	if _, err := fcntlLockHeld(dir + "/absent"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("probing an absent lock file gave %v, want a not-exist error", err)
	}

	// A lock taken by THIS process would not conflict with itself, so the held
	// case needs another process. The test re-runs its own binary as the
	// holder.
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperHoldsAnFcntlLock")
	cmd.Env = append(os.Environ(), "PATCHFACTS_LOCK_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	buf := make([]byte, 7)
	if _, err := stdout.Read(buf); err != nil {
		t.Fatalf("the helper did not report that it holds the lock: %v", err)
	}

	held, err = fcntlLockHeld(path)
	if err != nil {
		t.Fatalf("probe a locked file: %v", err)
	}
	if !held {
		t.Error("a file locked by another process read as free")
	}
}

// TestHelperHoldsAnFcntlLock is not a test. It is the child process of
// TestPackageLockProbe: it takes a real fcntl write lock and holds it.
func TestHelperHoldsAnFcntlLock(t *testing.T) {
	path := os.Getenv("PATCHFACTS_LOCK_PATH")
	if path == "" {
		t.Skip("not the lock-holding child process")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		t.Fatal(err)
	}
	fmt.Print("locked\n")
	time.Sleep(30 * time.Second)
}

// The count is not reported when the hold list cannot be read, because the
// contract says the count excludes every held package and a count taken without
// that list would be too high.
func TestCountIsNotReportedWhenTheHoldListCannotBeRead(t *testing.T) {
	node := capturedNode(t)
	node.showholdErr = errors.New("apt-mark: command not found")

	rec, err := node.collector().Collect(Options{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if rec.SecurityUpdateCount != nil {
		t.Errorf("SecurityUpdateCount = %d, want nil", *rec.SecurityUpdateCount)
	}
	if rec.HeldPackages != nil {
		t.Errorf("HeldPackages = %v, want nil: an empty list would read as no holds", rec.HeldPackages)
	}
	if rec.FieldErrors["heldPackages"] == "" || rec.FieldErrors["securityUpdateCount"] == "" {
		t.Errorf("FieldErrors = %v, want entries for heldPackages and securityUpdateCount", rec.FieldErrors)
	}
	if node.ran("apt-cache") {
		t.Error("the collector read the package policies although it could not exclude the held packages")
	}
}
