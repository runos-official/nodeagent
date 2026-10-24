package install

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/runos-official/nodeagent/version"
)

// MinimumVersion is the security floor built into this binary (FCR 1650). The
// updater never installs a release below it, with or without
// --allow-downgrade. v1.8.4 is the first release with the fix for the root
// command injection in the Kubernetes upgrade instruction (FCR 1648), so a
// lower target would reopen that hole. Raise this value when a later release
// fixes a security defect that an older release still has.
const MinimumVersion = "1.8.4"

// floorDecision is the outcome of the downgrade check for one update.
type floorDecision int

const (
	// floorAllow: the target is at or above the running version and the
	// built-in minimum. Install it.
	floorAllow floorDecision = iota
	// floorAllowDowngrade: the target is below the running version, the
	// caller passed --allow-downgrade, and the target is at or above the
	// built-in minimum. Install it and log the downgrade.
	floorAllowDowngrade
	// floorSkip: the advertised version is below the running version. Keep
	// the running binary and report it. A bare `runos update` never moves a
	// node down.
	floorSkip
	// floorRefuse: the target is below the built-in minimum, or it is an
	// explicit --version below the running version without --allow-downgrade.
	floorRefuse
)

// checkVersionFloor is the one decision function for the downgrade policy. It
// runs before any download. target and running are semantic versions with or
// without a leading "v". An unparseable running version (a local "dev" build,
// or a binary whose version cannot be read) leaves only the built-in minimum.
// advertised is true when the target came from the control plane, not from an
// explicit --version.
func checkVersionFloor(target, running string, advertised, allowDowngrade bool) (floorDecision, string) {
	belowMin, err := compareSemver(target, MinimumVersion)
	if err != nil {
		return floorRefuse, fmt.Sprintf("target version %q is not a semantic version: %v", target, err)
	}
	if belowMin < 0 {
		return floorRefuse, fmt.Sprintf("target %s is below the minimum version %s that this agent accepts; releases below it lack security fixes", target, MinimumVersion)
	}
	cmp, err := compareSemver(target, running)
	if err != nil || cmp >= 0 {
		return floorAllow, ""
	}
	if allowDowngrade {
		return floorAllowDowngrade, fmt.Sprintf("downgrading from %s to %s because --allow-downgrade was given", running, target)
	}
	if advertised {
		return floorSkip, fmt.Sprintf("the advertised version %s is older than the running version %s; keeping %s", target, running, running)
	}
	return floorRefuse, fmt.Sprintf("target %s is older than the running version %s; pass --allow-downgrade to move down on purpose", target, running)
}

// runningVersion returns the version of the agent that runs this update. It is
// the version compiled into this process. A local build reports "dev", so the
// version of the installed binary is the fallback. Declared as a var so tests
// can set it.
var runningVersion = func() string {
	if _, _, _, err := parseSemver(version.Version); err == nil {
		return version.Version
	}
	return getInstalledVersion()
}

// compareSemver compares two semantic versions by Semantic Versioning 2.0.0
// precedence: -1 when a < b, 0 when equal, 1 when a > b. A leading "v" is
// ignored, and so is build metadata after "+". A pre-release sorts below its
// release (1.8.5-rc.1 < 1.8.5).
func compareSemver(a, b string) (int, error) {
	ca, pa, _, err := parseSemver(a)
	if err != nil {
		return 0, err
	}
	cb, pb, _, err := parseSemver(b)
	if err != nil {
		return 0, err
	}
	for i := 0; i < 3; i++ {
		if ca[i] != cb[i] {
			if ca[i] < cb[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	return comparePrerelease(pa, pb), nil
}

// parseSemver splits a version into its numeric core, its pre-release
// identifiers and its build metadata.
func parseSemver(v string) ([3]uint64, []string, string, error) {
	var core [3]uint64
	s := strings.TrimPrefix(strings.TrimSpace(v), "v")
	build := ""
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s, build = s[:i], s[i+1:]
	}
	var pre []string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		if i == len(s)-1 {
			return core, nil, "", fmt.Errorf("%q has an empty pre-release", v)
		}
		pre = strings.Split(s[i+1:], ".")
		s = s[:i]
		for _, id := range pre {
			if id == "" {
				return core, nil, "", fmt.Errorf("%q has an empty pre-release identifier", v)
			}
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return core, nil, "", fmt.Errorf("%q is not MAJOR.MINOR.PATCH", v)
	}
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil || p == "" {
			return core, nil, "", fmt.Errorf("%q has a non-numeric part %q", v, p)
		}
		core[i] = n
	}
	return core, pre, build, nil
}

// comparePrerelease applies the Semantic Versioning 2.0.0 pre-release rules.
// No pre-release ranks above any pre-release. Numeric identifiers compare as
// numbers and rank below alphanumeric ones. A longer list wins a tie.
func comparePrerelease(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		na, errA := strconv.ParseUint(a[i], 10, 64)
		nb, errB := strconv.ParseUint(b[i], 10, 64)
		switch {
		case errA == nil && errB == nil:
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
		case errA == nil:
			return -1
		case errB == nil:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}
