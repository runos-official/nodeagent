package preflight

import (
	"fmt"
	"os"
	"strings"

	"github.com/runos-official/nodeagent/roslog"
)

// severity controls how a failing check is reported.
type severity int

const (
	sevBlock severity = iota // must be fixed; preflight fails (nothing is installed)
	sevWarn                  // advisory; preflight still passes but the user is told
)

// check is one preflight probe expressed as data, so the runner can order checks
// (cheap/local before network), collect ALL findings in a phase instead of
// stopping at the first, and distinguish blocking from advisory results.
//
// fn returns nil on pass, or an error whose message is the full, user-facing
// cause+remedy (multi-line is fine — it is indented by the reporter).
type check struct {
	name  string       // short stable id shown in the report, e.g. "kernel-modules"
	fn    func() error // nil = pass; non-nil = a finding
	sev   severity     // sevBlock or sevWarn
	net   bool         // true = needs network egress; runs in the network phase
	fatal bool         // fatal prerequisite: a failure stops preflight immediately
}

// finding is a check that did not pass.
type finding struct {
	name string
	err  error
}

// runPreflightChecks runs the whole battery and reports ALL problems at once, so
// the user can fix everything in a single pass rather than rerun-fix-rerun for
// each. The exception is fatal prerequisites (root, systemd, a real kernel,
// supported OS): if one of those fails the rest are meaningless, so we stop
// there. Returns a non-nil error if any blocking check failed (the caller just
// needs to exit non-zero — the detail has already been printed here).
func runPreflightChecks() error {
	if os.Getenv("RUNOS_DEV_SKIP_PREFLIGHT") == "1" {
		fmt.Println("RUNOS_DEV_SKIP_PREFLIGHT=1 is set, skipping preflight checks")
		return nil
	}

	checks := preflightChecks()

	// 1. Fatal prerequisites, fail-fast and in declared order.
	for _, c := range checks {
		if !c.fatal {
			continue
		}
		if err := c.fn(); err != nil {
			reportFatal(c, err)
			return fmt.Errorf("%s: %w", c.name, err)
		}
	}

	// 2 + 3. Local checks first (no network), then network checks. Within each
	// phase we run every check and collect findings rather than stopping early.
	var blocks, warns []finding
	collect := func(wantNet bool) {
		for _, c := range checks {
			if c.fatal || c.net != wantNet {
				continue
			}
			if err := c.fn(); err != nil {
				if c.sev == sevWarn {
					warns = append(warns, finding{c.name, err})
				} else {
					blocks = append(blocks, finding{c.name, err})
				}
			}
		}
	}
	collect(false) // local
	collect(true)  // network

	return report(blocks, warns)
}

// reportFatal prints a single fatal-prerequisite failure (the rest of preflight
// was not run because it would be meaningless).
func reportFatal(c check, err error) {
	roslog.E("preflight check failed", err, "check", c.name)
	fmt.Fprintf(os.Stderr, "\n%s%s✗ FAILED:%s %s\n  %s\n",
		roslog.ColorBold, roslog.ColorRed, roslog.ColorReset, c.name, indentLines(err.Error()))
	fmt.Fprintf(os.Stderr, "\n  %s\n", roslog.SupportLine)
}

// report prints all collected warnings and blocking findings. Warnings come
// first (advisory) so the blockers — the things the user must act on — are the
// last thing on screen, followed by a single support line. Returns an error iff
// there were blocking findings.
func report(blocks, warns []finding) error {
	for _, w := range warns {
		fmt.Fprintf(os.Stderr, "\n%s⚠ WARNING [%s]:%s %s\n",
			roslog.ColorYellow, w.name, roslog.ColorReset, indentLines(w.err.Error()))
	}

	if len(blocks) == 0 {
		return nil
	}

	for _, b := range blocks {
		roslog.E("preflight check failed", b.err, "check", b.name)
		fmt.Fprintf(os.Stderr, "\n%s%s✗ BLOCKED [%s]:%s %s\n",
			roslog.ColorBold, roslog.ColorRed, b.name, roslog.ColorReset, indentLines(b.err.Error()))
	}
	fmt.Fprintf(os.Stderr, "\n%s%d blocking issue(s) must be fixed before installation — nothing has been installed.%s\n",
		roslog.ColorBold, len(blocks), roslog.ColorReset)
	fmt.Fprintf(os.Stderr, "%s\n", roslog.SupportLine)

	return fmt.Errorf("%d preflight check(s) failed", len(blocks))
}

// indentLines indents the 2nd..nth lines of a (possibly multi-line) message so
// remedies line up under the check header.
func indentLines(s string) string {
	return strings.ReplaceAll(s, "\n", "\n  ")
}

// preflightChecks is the ordered registry. Order within a phase is preserved.
// Fatal prerequisites run first (fail-fast); then local block+warn checks; then
// network block+warn checks. Replaced/weak legacy checks are intentionally
// absent (superseded by the stronger versions noted inline).
func preflightChecks() []check {
	return []check{
		// ---- fatal prerequisites (fail-fast, in order) ----
		{name: "root", fn: checkRoot, sev: sevBlock, fatal: true},
		{name: "systemd-init", fn: checkSystemdIsInit, sev: sevBlock, fatal: true},
		{name: "proc-sys-mounted", fn: checkProcSysFsMounted, sev: sevBlock, fatal: true},
		{name: "cpu-arch", fn: checkArch, sev: sevBlock, fatal: true},
		{name: "os-version", fn: checkOSVersion, sev: sevBlock, fatal: true},
		{name: "virtualization", fn: checkContainerOrWSL, sev: sevBlock, fatal: true},
		{name: "base-tooling", fn: checkCurlAndBaseTooling, sev: sevBlock, fatal: true},

		// ---- local, blocking ----
		{name: "cpu-count", fn: checkCPUCount, sev: sevBlock},
		{name: "ram", fn: checkRAM, sev: sevBlock},
		{name: "disk-space", fn: checkVarMountSpaceAndInodes, sev: sevBlock}, // supersedes checkDiskSpace
		{name: "mount-flags", fn: checkTmpExecAndDataMounts, sev: sevBlock},
		{name: "swap", fn: checkSwap, sev: sevBlock},
		{name: "swap-persistence", fn: checkSwapPersistence, sev: sevBlock},
		{name: "ports-free", fn: checkPortsFree, sev: sevBlock},
		{name: "reboot-required", fn: checkRebootRequired, sev: sevBlock},
		{name: "package-locks", fn: checkPackageManagerLocks, sev: sevBlock},
		{name: "kernel-modules", fn: checkKernelModulesRealLoad, sev: sevBlock}, // supersedes dry-run checkKernelModules
		{name: "cgroup-v2", fn: checkCgroupV2WithControllers, sev: sevBlock},
		{name: "sysctls-writable", fn: checkRequiredSysctlsWritable, sev: sevBlock},
		{name: "existing-k8s", fn: checkExistingKubernetesAndLeftovers, sev: sevBlock}, // supersedes checkConflictingServices
		{name: "hostname", fn: checkHostnameValidAndPersistent, sev: sevBlock},
		{name: "machine-id", fn: checkMachineId, sev: sevBlock},
		{name: "resolv-conf", fn: checkResolvConfAndNsswitch, sev: sevBlock},
		{name: "l1sec-ca-file", fn: checkL1SecCAFileValid, sev: sevBlock},
		{name: "already-registered", fn: checkAlreadyRegistered, sev: sevBlock},
		{name: "install-lock", fn: checkInstallLock, sev: sevBlock},
		{name: "capabilities", fn: checkLinuxCapabilities, sev: sevBlock},
		{name: "immutable-paths", fn: checkImmutableTargetPaths, sev: sevBlock},
		{name: "wireguard-subnet", fn: checkWireguardSubnetOverlap, sev: sevBlock},
		{name: "cloud-init", fn: checkCloudInitComplete, sev: sevBlock},
		// clock is local (timedatectl) but must run before the network/TLS checks.
		{name: "clock-skew", fn: checkClockSkew, sev: sevBlock},

		// ---- local, advisory ----
		{name: "ram-pressure", fn: checkRamAvailableAndPressure, sev: sevWarn},
		{name: "firewall-posture", fn: checkHostFirewallEgressPosture, sev: sevWarn},
		{name: "rp-filter", fn: checkRpFilterAndMultiHome, sev: sevWarn},
		{name: "entropy", fn: checkEntropyAvailable, sev: sevWarn},
		{name: "etcd-fsync", fn: checkEtcdDiskFsyncLatency, sev: sevWarn},
		{name: "resource-limits", fn: checkResourceLimits, sev: sevWarn},

		// ---- network, blocking ----
		{name: "dns-resolution", fn: checkDNSResolution, sev: sevBlock, net: true},
		{name: "proxy-direct-path", fn: checkProxyAndNodewardDirectPath, sev: sevBlock, net: true},
		{name: "nodeward-reachable", fn: checkNodewardReachable, sev: sevBlock, net: true},
		{name: "nodeward-highport", fn: checkNodewardHighPortVs443, sev: sevBlock, net: true},
		{name: "nodeward-tls-pin", fn: checkNodewardTlsHandshakePinned, sev: sevBlock, net: true},
		{name: "egress-endpoints", fn: checkEgressEndpointSetComplete, sev: sevBlock, net: true}, // supersedes curl checkNetworkConnectivity
		{name: "captive-portal", fn: checkCaptivePortalContentCanary, sev: sevBlock, net: true},
		{name: "apt-sources", fn: checkAptSourcesUsable, sev: sevBlock, net: true}, // supersedes checkBrokenAptSources

		// ---- network, advisory ----
		{name: "dns-answer-sanity", fn: checkDnsAnswerSanity, sev: sevWarn, net: true},
		{name: "wireguard-udp-egress", fn: checkOutboundUdpForWireguard, sev: sevWarn, net: true},
		{name: "nat-collision", fn: checkNATEndpointCollision, sev: sevWarn, net: true},
	}
}
