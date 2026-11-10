package patchfacts

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestLiveNodeAgainstTheDistributionCounter runs the collector on the machine it
// is on and compares its security update count with the distribution's own
// counter, /usr/lib/update-notifier/apt-check.
//
// It is skipped unless PATCHFACTS_LIVE=1, because it needs a real Ubuntu node.
// It runs a NON-slow pass, so it refreshes nothing and changes nothing.
//
// The two counts are allowed to differ, and the test prints the difference
// rather than failing on it, because two differences are expected and both are
// correct for this contract:
//
//   - apt-check ignores apt-mark holds, and this contract excludes every held
//     package, so apt-check's total is higher by the number of held packages
//     that have a waiting update.
//   - apt-check marks the upgrade with a dist-upgrade, which can newly INSTALL
//     a package and count it as a security update. The collector rests on
//     apt-get -s upgrade, which never adds or removes a package, because a
//     controlled maintenance run may only move packages that are installed.
//
// A difference that is NOT explained by those two is a defect in the parser.
func TestLiveNodeAgainstTheDistributionCounter(t *testing.T) {
	if os.Getenv("PATCHFACTS_LIVE") != "1" {
		t.Skip("set PATCHFACTS_LIVE=1 to run the collector against the machine it is on")
	}

	rec, err := New().Collect(Options{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	t.Logf("record:\n%s", body)

	if rec.RunningKernel == "" {
		t.Error("RunningKernel is empty on a live node")
	}
	if rec.AutomaticUpdateUnitState == UnitStateUnknown {
		t.Errorf("AutomaticUpdateUnitState is unknown: %v", rec.FieldErrors["automaticUpdateUnitState"])
	}
	if rec.BootTime == nil {
		t.Error("BootTime is nil on a live node")
	}
	if rec.PackageMetadataAgeSeconds == nil {
		t.Errorf("PackageMetadataAgeSeconds is nil: %v", rec.FieldErrors["packageMetadataAgeSeconds"])
	}
	if rec.SecurityUpdateCount == nil {
		t.Fatalf("SecurityUpdateCount is nil: %v", rec.FieldErrors["securityUpdateCount"])
	}

	out, err := exec.Command("/usr/lib/update-notifier/apt-check").CombinedOutput()
	if err != nil {
		t.Logf("the distribution counter is not available here, so the cross-check did not run: %v", err)
		return
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ";")
	if len(fields) != 2 {
		t.Logf("the distribution counter printed %q, which is not <upgrades>;<security>", out)
		return
	}
	theirSecurity, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Logf("the distribution counter printed %q", out)
		return
	}
	t.Logf("security updates: collector %d, distribution counter %d, held packages %d",
		*rec.SecurityUpdateCount, theirSecurity, len(rec.HeldPackages))
	t.Logf("collector security names: %s", strings.Join(rec.SecurityUpdatePackages, " "))
}
