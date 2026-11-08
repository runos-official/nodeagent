package patchfacts

import (
	"fmt"
	"strings"
)

// Command keys. The collector asks for a row by key and can run nothing else.
const (
	cmdUname             = "uname"
	cmdDpkgKernels       = "dpkg-kernels"
	cmdAptMarkShowhold   = "apt-mark-showhold"
	cmdDpkgHeldVersions  = "dpkg-held-versions"
	cmdAptSimulateUpdate = "apt-simulate-upgrade"
	cmdAptPolicyIndexes  = "apt-policy-indexes"
	cmdAptPolicyPackages = "apt-policy-packages"
	cmdAptConfigPeriodic = "apt-config-periodic"
	cmdSystemctlShow     = "systemctl-show-unit"
	cmdAptRefresh        = "apt-refresh"
)

// command is one row of the collector's command table.
type command struct {
	// key names the row. The collector runs a row by key.
	key string
	// name is the program, and args its fixed arguments.
	name string
	args []string
	// takesNames is true when the collector appends package or unit names to
	// args. An appended name is validated by safeName before it is used.
	takesNames bool
	// writes is true for the one row that changes the node.
	writes bool
	// purpose says, in one sentence, what the row reads.
	purpose string
}

// commandTable is EVERY command this package can run. The collector runs a row
// by key and refuses an unknown key, so this table is the whole surface the
// collector presents to the node.
//
// Exactly one row writes: apt-refresh, the package metadata refresh. No row
// installs, removes, purges, reinstalls, holds or unholds a package, and no row
// upgrades one. The apt upgrade row carries -s, so apt only simulates.
//
// TestCommandTableChangesNothing walks this table and fails on any row that
// breaks those rules, so a later story cannot add a mutating command without
// the test saying so.
var commandTable = []command{{
	key:     cmdUname,
	name:    "uname",
	args:    []string{"-r"},
	purpose: "read the running kernel release",
}, {
	key:     cmdDpkgKernels,
	name:    "dpkg-query",
	args:    []string{"-W", "-f=${Package} ${Version} ${db:Status-Status}\n", "linux-image-*"},
	purpose: "read the installed kernel packages and their status from dpkg, which is the authority on what is installed",
}, {
	key:     cmdAptMarkShowhold,
	name:    "apt-mark",
	args:    []string{"showhold"},
	purpose: "read the names of the held packages",
}, {
	key:        cmdDpkgHeldVersions,
	name:       "dpkg-query",
	args:       []string{"-W", "-f=${Package} ${Version} ${db:Status-Status}\n"},
	takesNames: true,
	purpose:    "read the version each held package is held at",
}, {
	key:  cmdAptSimulateUpdate,
	name: "apt-get",
	// -s simulates, so apt changes nothing. Debug::NoLocking lets the call run
	// beside another package manager without taking a lock. upgrade (never
	// dist-upgrade) is the right basis, because it neither adds nor removes a
	// package, which matches what a controlled maintenance run may do.
	args:    []string{"-s", "-o", "Debug::NoLocking=true", "upgrade"},
	purpose: "read which installed packages have a waiting update",
}, {
	key:     cmdAptPolicyIndexes,
	name:    "apt-cache",
	args:    []string{"policy"},
	purpose: "read each package index with the origin and archive of its own Release file, so a security pocket is named by the archive rather than by the spelling in sources.list",
}, {
	key:        cmdAptPolicyPackages,
	name:       "apt-cache",
	args:       []string{"policy"},
	takesNames: true,
	purpose:    "read every available version of each waiting package and the indexes it sits in",
}, {
	key:     cmdAptConfigPeriodic,
	name:    "apt-config",
	args:    []string{"dump", "APT::Periodic"},
	purpose: "read the apt periodic settings with apt's own reader, so defaults and quoting are apt's and not ours",
}, {
	key:        cmdSystemctlShow,
	name:       "systemctl",
	args:       []string{"show", "--property=Id,LoadState,UnitFileState,ActiveState"},
	takesNames: true,
	purpose:    "read the load, unit file and active state of one automatic package unit as a key and value list",
}, {
	key:  cmdAptRefresh,
	name: "apt-get",
	// THE ONLY WRITING ROW. RunOS masks the automatic package timers at node
	// install, so nothing on a running node refreshes the package metadata and
	// a count computed from metadata last refreshed at install reads zero
	// forever. This row is what keeps the count true. A reviewer who removes it
	// to make the collector read-only restores that permanent zero.
	//
	// The zero lock timeout makes apt fail at once instead of waiting out
	// another package manager. It is the second guard, not the first: the
	// collector checks the locks in its own step before it reaches this row.
	args:    []string{"-o", "DPkg::Lock::Timeout=0", "-o", "Acquire::Retries=1", "update"},
	writes:  true,
	purpose: "refresh the package metadata, on a slow pass only, when no package manager lock is held",
}}

// automaticUpdateUnits are the units that install packages on their own when
// nobody masks them.
//
// apt itself ships the four apt-daily units, so the names are the same on
// Debian and on Ubuntu, and unattended-upgrades ships the fifth. The names were
// confirmed against the apt package's own file list and read back on two
// running Ubuntu 24.04 nodes before they were pinned here, because a name that
// does not exist on a supported release reads as unknown on every node of that
// release, and unknown looks like a transient fault rather than a wrong
// constant.
var automaticUpdateUnits = []string{
	"apt-daily.timer",
	"apt-daily.service",
	"apt-daily-upgrade.timer",
	"apt-daily-upgrade.service",
	"unattended-upgrades.service",
}

// File paths the collector reads. Reading a file changes nothing, so no entry
// here writes. The list is here beside the command table so one place answers
// "what does the collector touch".
const (
	pathProcStat          = "/proc/stat"
	pathRebootRequired    = "/var/run/reboot-required"
	pathRebootRequiredPkg = "/var/run/reboot-required.pkgs"
	pathOSRelease         = "/etc/os-release"
	pathAptHistory        = "/var/log/apt/history.log"
	pathAptHistoryRotated = "/var/log/apt/history.log.1.gz"
	pathAptLists          = "/var/lib/apt/lists"

	// pathUpdateSuccessStamp is touched by /etc/apt/apt.conf.d/15update-stamp,
	// an APT::Update::Post-Invoke-Success hook that the update-notifier-common
	// package installs. It therefore records the last SUCCESSFUL package
	// metadata refresh, on the node's own clock, whoever ran it, and it does
	// not depend on the masked apt-daily timer.
	pathUpdateSuccessStamp = "/var/lib/apt/periodic/update-success-stamp"
)

// lockPaths are the package manager locks the collector probes before it
// refreshes the metadata. apt and dpkg both take these with fcntl record
// locks, so the probe asks fcntl and not flock: the two lock kinds do not see
// each other on Linux.
var lockPaths = []string{
	"/var/lib/dpkg/lock-frontend",
	"/var/lib/dpkg/lock",
	"/var/lib/apt/lists/lock",
	"/var/cache/apt/archives/lock",
}

// lookupCommand returns the table row with this key.
func lookupCommand(key string) (command, error) {
	for _, c := range commandTable {
		if c.key == key {
			return c, nil
		}
	}
	return command{}, fmt.Errorf("patchfacts: %q is not a row of the command table", key)
}

// safeName reports whether s is safe to append to a command's arguments. A
// package or unit name comes from the node's own output, never from a request,
// and this keeps a surprising name from being read as an option.
func safeName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '+', r == '-', r == '_', r == ':', r == '@':
		default:
			return false
		}
	}
	return true
}
