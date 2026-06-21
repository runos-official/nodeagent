package preflight

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/runos-official/nodeagent/roslog"
)

// ---------------------------------------------------------------------------
// group: system / PREFIX token: sys
//
// All exported check functions return nil on pass, or an error whose message is
// the FULL user-facing text (one-line cause, blank line, concrete remedy). The
// guiding rule is "no false positives": when a fact cannot be determined
// reliably (missing tool/file, ambiguous output, non-Linux quirk), the check
// returns nil (optionally roslog.W) rather than blocking an otherwise fine host.
// ---------------------------------------------------------------------------

// sysCmdTimeout bounds every external command we shell out to so a wedged tool
// can never hang preflight.
const sysCmdTimeout = 6 * time.Second

// sysRunCmd runs name+args with a hard timeout, returning combined-ish stdout
// (trimmed), the raw error, and whether the command was found+executable at
// all. ok=false means "could not run" (tool missing / exec error) and callers
// must treat that as "unknown", not "failed".
func sysRunCmd(name string, args ...string) (out string, err error, ok bool) {
	if _, lerr := exec.LookPath(name); lerr != nil {
		return "", lerr, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), sysCmdTimeout)
	defer cancel()
	b, rerr := exec.CommandContext(ctx, name, args...).Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", ctx.Err(), false
	}
	return strings.TrimSpace(string(b)), rerr, true
}

// sysReadFileTrim reads a small pseudo-file and trims it; ok=false on any read
// error (treated as "unknown").
func sysReadFileTrim(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// sysFileExists reports whether path exists (any type).
func sysFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// sysModuleLoaded reports whether a kernel module is currently present, checking
// the canonical sources: /sys/module/<mod>, /proc/modules, and (for filesystem
// modules like overlay) /proc/filesystems.
func sysModuleLoaded(mod string) bool {
	if sysFileExists("/sys/module/" + mod) {
		return true
	}
	if data, ok := sysReadFileTrim("/proc/modules"); ok {
		for _, line := range strings.Split(data, "\n") {
			if f := strings.Fields(line); len(f) > 0 && f[0] == mod {
				return true
			}
		}
	}
	if fs, ok := sysReadFileTrim("/proc/filesystems"); ok {
		for _, line := range strings.Split(fs, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[len(fields)-1] == mod {
				return true
			}
		}
	}
	return false
}

// sysModprobe attempts a REAL load of mod (preflight runs as root). Returns
// whether the module ended up loaded and the modprobe error (if any).
func sysModprobe(mod string) (loaded bool, err error) {
	if sysModuleLoaded(mod) {
		return true, nil
	}
	_, merr, ok := sysRunCmd("modprobe", mod)
	if !ok {
		// modprobe itself missing/timed out -> we cannot assert; report as
		// not-loaded with the error so callers can decide (they treat unknown
		// tooling as non-blocking).
		return sysModuleLoaded(mod), merr
	}
	return sysModuleLoaded(mod), merr
}

// sysIsContainer best-effort detects whether we are inside a container, using
// systemd-detect-virt -c first, then falling back to container hint files. The
// bool result is the verdict; ok=false means we genuinely could not tell.
func sysIsContainer() (kind string, isContainer bool, ok bool) {
	if out, _, run := sysRunCmd("systemd-detect-virt", "-c"); run {
		v := strings.TrimSpace(out)
		// exit code is nonzero ("none") when not a container; sysRunCmd returns
		// empty out in that case. A non-empty, non-"none" value is the type.
		if v != "" && v != "none" {
			return v, true, true
		}
		// Ran cleanly and said "none"/empty -> confidently not a container.
		if run {
			ok = true
		}
	}
	// Fallback hints (used only to ADD confidence, never to override a clean
	// "none" above unless detect-virt was unavailable).
	if !ok {
		if sysFileExists("/.dockerenv") {
			return "docker", true, true
		}
		if data, fok := sysReadFileTrim("/proc/1/cgroup"); fok {
			low := strings.ToLower(data)
			for _, hint := range []string{"docker", "lxc", "kubepods", "containerd", "podman"} {
				if strings.Contains(low, hint) {
					return hint, true, true
				}
			}
		}
	}
	return "", false, ok
}

// sysIsUnprivilegedUserns reports whether /proc/self/uid_map indicates an
// unprivileged user namespace (root not mapped at 0, or not a full-range map).
// ok=false when the map is absent/unreadable.
func sysIsUnprivilegedUserns() (unpriv bool, ok bool) {
	data, fok := sysReadFileTrim("/proc/self/uid_map")
	if !fok || data == "" {
		return false, false
	}
	// Format: "<inside> <outside> <count>". A normal/host map is "0 0 4294967295".
	fields := strings.Fields(strings.Split(data, "\n")[0])
	if len(fields) != 3 {
		return false, false
	}
	inside, e1 := strconv.ParseUint(fields[0], 10, 64)
	outside, e2 := strconv.ParseUint(fields[1], 10, 64)
	count, e3 := strconv.ParseUint(fields[2], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return false, false
	}
	// Unprivileged iff uid 0 inside does not map to uid 0 outside, or the range
	// is not effectively full.
	if inside != 0 || outside != 0 || count < 4294967295 {
		return true, true
	}
	return false, true
}

// sysIsWSL detects Windows Subsystem for Linux from kernel osrelease/version
// strings.
func sysIsWSL() bool {
	for _, p := range []string{"/proc/sys/kernel/osrelease", "/proc/version"} {
		if s, ok := sysReadFileTrim(p); ok {
			l := strings.ToLower(s)
			if strings.Contains(l, "microsoft") || strings.Contains(l, "wsl") {
				return true
			}
		}
	}
	return false
}

// sysSysctlPath turns a dotted sysctl key into its /proc/sys path.
func sysSysctlPath(key string) string {
	return "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
}

// sysSysctlWritable attempts a no-op write-back of a sysctl (read current value,
// write it back unchanged). Returns whether the write succeeded and the error.
// ok=false means the key does not exist / is unreadable (cannot assert).
func sysSysctlWritable(key string) (writable bool, err error, ok bool) {
	path := sysSysctlPath(key)
	cur, fok := sysReadFileTrim(path)
	if !fok {
		return false, nil, false
	}
	// Write the same value back: a permitted write is a harmless no-op; a denied
	// one returns EROFS/EPERM exactly as the real installer would hit.
	werr := os.WriteFile(path, []byte(cur+"\n"), 0644)
	if werr != nil {
		return false, werr, true
	}
	return true, nil, true
}

// sysStatfsType returns the filesystem magic type at path via statfs.
func sysStatfsType(path string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Type), true
}

// sysProcSysReadOnly reports whether /proc/sys is mounted read-only (ST_RDONLY).
func sysProcSysReadOnly() (ro bool, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/proc/sys", &st); err != nil {
		return false, false
	}
	const stRdonly = 1 // ST_RDONLY / MS_RDONLY
	return st.Flags&stRdonly != 0, true
}

// sysGlobReadAll concatenates the contents of all files matching the given glob
// patterns (used to scan modprobe.d and sysctl.d drop-in directories).
func sysGlobReadAll(patterns ...string) string {
	var sb strings.Builder
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			if b, err := os.ReadFile(m); err == nil {
				sb.Write(b)
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String()
}

// sysModuleBlacklisted reports the first drop-in file that blacklists mod, or ""
// if none. Scans /etc, /lib, /usr/lib and /run modprobe.d trees.
func sysModuleBlacklisted(mod string) string {
	dirs := []string{
		"/etc/modprobe.d", "/lib/modprobe.d", "/usr/lib/modprobe.d", "/run/modprobe.d",
	}
	needle := "blacklist " + mod
	for _, d := range dirs {
		matches, _ := filepath.Glob(filepath.Join(d, "*.conf"))
		for _, m := range matches {
			b, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "#") {
					continue
				}
				if line == needle || strings.HasPrefix(line, needle+" ") {
					return m
				}
			}
		}
	}
	return ""
}

// sysReadCapEff parses the hex CapEff/CapBnd field from /proc/self/status.
func sysReadCapField(field string) (uint64, bool) {
	data, ok := sysReadFileTrim("/proc/self/status")
	if !ok {
		return 0, false
	}
	for _, line := range strings.Split(data, "\n") {
		if strings.HasPrefix(line, field+":") {
			val := strings.TrimSpace(strings.TrimPrefix(line, field+":"))
			n, err := strconv.ParseUint(val, 16, 64)
			if err != nil {
				return 0, false
			}
			return n, true
		}
	}
	return 0, false
}

// sysHasCap reports whether capability bit cap is set in the bitmask.
func sysHasCap(mask uint64, cap uint) bool {
	return mask&(1<<cap) != 0
}

// Capability bit numbers (from <linux/capability.h>).
const (
	sysCapNetAdmin  = 12
	sysCapNetRaw    = 13
	sysCapSysModule = 16
	sysCapSysAdmin  = 21
)

// checkSystemdIsInit verifies that systemd is PID 1. RunOS drives the agent and
// Kubernetes via systemd units; without it everything downstream (unit
// install/start) fails with confusing "Failed to connect to bus" / "system has
// not been booted with systemd" errors. Prevents that cryptic class of failure
// on sysvinit/initless/minimal-container images.
func checkSystemdIsInit() error {
	// Primary signal, matches libsystemd sd_booted().
	if sysFileExists("/run/systemd/system") {
		return nil
	}

	// Corroborate before blocking: identify PID 1.
	comm := "unknown"
	if c, ok := sysReadFileTrim("/proc/1/comm"); ok && c != "" {
		comm = c
	}
	if link, err := os.Readlink("/proc/1/exe"); err == nil {
		if strings.HasSuffix(link, "/systemd") || filepath.Base(link) == "systemd" {
			// PID 1 is systemd but /run/systemd/system not yet present (very early
			// boot). Do not block.
			return nil
		}
	}
	if comm == "systemd" {
		return nil
	}

	return fmt.Errorf("systemd is not the init system on this node (PID 1 is %q). RunOS manages the agent and Kubernetes via systemd units and cannot run without it.\n\nUse a standard systemd-based Ubuntu image (not a minimal/sysvinit or initless container). This is an environment prerequisite, not a RunOS error.", comm)
}

// checkProcSysFsMounted verifies the core kernel pseudo-filesystems (/proc,
// /sys, /sys/fs/cgroup) are present and mounted. If they are missing, every
// dependent check (cgroups, sysctls, modules, swap) would silently no-op into a
// false PASS. This fails closed on chroots and broken/minimal containers,
// preventing a "preflight passed but Kubernetes never starts" outcome.
func checkProcSysFsMounted() error {
	var missing []string
	if !sysFileExists("/proc/1/stat") {
		missing = append(missing, "/proc (no /proc/1/stat)")
	}
	if !sysFileExists("/proc/meminfo") {
		missing = append(missing, "/proc/meminfo")
	}
	if !sysFileExists("/proc/swaps") {
		missing = append(missing, "/proc/swaps")
	}
	if !sysFileExists("/sys/kernel") {
		missing = append(missing, "/sys")
	}
	if _, ok := sysStatfsType("/sys/fs/cgroup"); !ok {
		missing = append(missing, "/sys/fs/cgroup (statfs failed)")
	}

	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("core kernel filesystems are not properly mounted on this node (missing: %s), so RunOS can neither validate the environment nor run Kubernetes. This indicates a chroot or broken/minimal container.\n\nRun on a full VM or properly initialized host. This is an environment prerequisite, not a RunOS error.", strings.Join(missing, ", "))
}

// checkContainerOrWSL blocks when the node is WSL, an unprivileged container, or
// a container that genuinely cannot load modules / set sysctls. For a detected
// container it performs a REAL modprobe of br_netfilter and a real sysctl
// write-back; only an actual EPERM/EROFS (not mere detection) blocks. Prevents
// the late failure where kubeadm/CNI cannot configure pod networking inside a
// constrained container.
func checkContainerOrWSL() error {
	if sysIsWSL() {
		return fmt.Errorf("this node is WSL (Windows Subsystem for Linux) and cannot load the kernel modules or set the sysctls Kubernetes requires (br_netfilter, overlay, net.ipv4.ip_forward). RunOS needs a full VM or bare-metal host.\n\nProvision a real Ubuntu VM (KVM/cloud instance), then re-run. This is an environment prerequisite, not a RunOS error.")
	}

	kind, isContainer, ok := sysIsContainer()
	if !ok || !isContainer {
		// Either confidently not a container, or we could not tell. In both
		// cases do not block here (capabilities/sysctl/module checks cover the
		// genuinely-broken cases).
		return nil
	}

	// Unprivileged user namespace inside a container is a hard blocker.
	if unpriv, uok := sysIsUnprivilegedUserns(); uok && unpriv {
		return fmt.Errorf("this node is an unprivileged %s container (root is not mapped to host uid 0), so it cannot load kernel modules or set the sysctls Kubernetes requires (br_netfilter, overlay, net.ipv4.ip_forward). RunOS needs a full VM or bare-metal host.\n\nProvision a real Ubuntu VM (KVM/cloud instance), then re-run. This is an environment prerequisite, not a RunOS error.", kind)
	}

	// Privileged-looking container: prove unusability with a real load + write
	// before blocking. Only EPERM/EROFS-style failures count.
	loaded, mErr := sysModprobe("br_netfilter")
	writable, wErr, wOk := sysSysctlWritable("net.ipv4.ip_forward")

	moduleBlocked := !loaded && mErr != nil && sysErrIsPerm(mErr)
	sysctlBlocked := wOk && !writable && wErr != nil && sysErrIsPerm(wErr)

	if moduleBlocked || sysctlBlocked {
		var why string
		switch {
		case moduleBlocked && sysctlBlocked:
			why = "modprobe br_netfilter and the net.ipv4.ip_forward sysctl write both failed with permission errors"
		case moduleBlocked:
			why = fmt.Sprintf("modprobe br_netfilter failed (%v)", mErr)
		default:
			why = fmt.Sprintf("the net.ipv4.ip_forward sysctl write failed (%v)", wErr)
		}
		return fmt.Errorf("this node is a %s container and cannot configure the kernel networking Kubernetes requires: %s. RunOS needs a full VM or bare-metal host.\n\nProvision a real Ubuntu VM (KVM/cloud instance), then re-run. This is an environment prerequisite, not a RunOS error.", kind, why)
	}

	// Container detected but it can actually load modules and write sysctls
	// (e.g. a fully --privileged unconfined container). Do not block; just note.
	roslog.W("running inside a container; module load + sysctl write-back succeeded, allowing", nil, "kind", kind)
	return nil
}

// sysErrIsPerm reports whether err is a permission/read-only-fs style error
// (EPERM, EACCES, EROFS), the signature of a constrained container/hardened
// host as opposed to a transient or "not found" failure.
func sysErrIsPerm(err error) bool {
	if err == nil {
		return false
	}
	if os.IsPermission(err) {
		return true
	}
	var errno syscall.Errno
	if e, ok := err.(syscall.Errno); ok {
		errno = e
	} else if pe, ok := err.(*os.PathError); ok {
		if e, ok2 := pe.Err.(syscall.Errno); ok2 {
			errno = e
		}
	}
	if errno == syscall.EPERM || errno == syscall.EACCES || errno == syscall.EROFS {
		return true
	}
	// Fall back to string sniffing for modprobe's exec error text.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "operation not permitted") ||
		strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "read-only")
}

// checkCgroupV2WithControllers verifies the node uses the cgroup v2 unified
// hierarchy with the cpu, memory and pids controllers delegated. cgroup v1 (or
// v2 missing the memory/cpu controllers, a common nested-virt gotcha) makes the
// kubelet refuse to start or silently fail to enforce limits. Prevents that
// opaque kubelet cgroup-driver failure.
func checkCgroupV2WithControllers() error {
	const cgroup2Magic = 0x63677270 // "cgrp" / cgroup2fs

	t, ok := sysStatfsType("/sys/fs/cgroup")
	if !ok {
		// Cannot stat: proc-sys-mounted (a fatal prereq) already covers a truly
		// broken mount; do not double-block on ambiguity here.
		roslog.W("could not statfs /sys/fs/cgroup; skipping cgroup v2 check", nil)
		return nil
	}
	if t != cgroup2Magic {
		return fmt.Errorf("this node uses cgroup v1 (or hybrid) at /sys/fs/cgroup, but RunOS/Kubernetes require cgroup v2 unified with cpu+memory delegated.\n\nBoot with cgroup v2 (default on Ubuntu 22.04+): remove any 'systemd.unified_cgroup_hierarchy=0' kernel arg from /etc/default/grub, run 'sudo update-grub', and reboot. This is an environment prerequisite, not a RunOS error.")
	}

	controllers, cok := sysReadFileTrim("/sys/fs/cgroup/cgroup.controllers")
	if !cok {
		roslog.W("cgroup v2 detected but /sys/fs/cgroup/cgroup.controllers unreadable; skipping controller check", nil)
		return nil
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(controllers) {
		have[c] = true
	}
	var missing []string
	for _, need := range []string{"cpu", "memory", "pids"} {
		if !have[need] {
			missing = append(missing, need)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("this node has cgroup v2 but is missing required controllers (missing: %s; present: %s). RunOS/Kubernetes need cpu+memory+pids delegated.\n\nIf 'memory' is missing this is usually a kernel without the memory controller, or a nested/managed host that did not delegate it. Boot a kernel with these controllers (default on Ubuntu 22.04+) and ensure they appear in /sys/fs/cgroup/cgroup.controllers. This is an environment prerequisite, not a RunOS error.", strings.Join(missing, ", "), strings.TrimSpace(controllers))
	}
	return nil
}

// checkKernelModulesRealLoad performs a REAL load of every module Kubernetes/CNI
// needs (br_netfilter, overlay, nf_conntrack, wireguard) and verifies each is
// actually present, because a dry-run can pass while the real load fails. It
// also names the precise cause (missing /lib/modules tree, blacklist,
// kernel.modules_disabled, Secure Boot lockdown). Prevents the "CNI/overlayfs
// not working, no obvious reason" failure on stripped kernels / hardened hosts.
func checkKernelModulesRealLoad() error {
	rel := unameRelease()

	// Definitive blocker: no modules tree for the running kernel (common after a
	// kernel upgrade without linux-modules-extra, or on minimal cloud images).
	modulesDep := fmt.Sprintf("/lib/modules/%s/modules.dep", rel)
	modulesTreeMissing := rel != "unknown" && !sysFileExists(modulesDep)

	// Global kill switches that make a real load impossible.
	modulesDisabled := false
	if v, ok := sysReadFileTrim("/proc/sys/kernel/modules_disabled"); ok && v == "1" {
		modulesDisabled = true
	}
	lockdown := ""
	if v, ok := sysReadFileTrim("/sys/kernel/security/lockdown"); ok {
		// Active mode is wrapped in [brackets].
		if strings.Contains(v, "[integrity]") || strings.Contains(v, "[confidentiality]") {
			lockdown = strings.TrimSpace(v)
		}
	}

	required := []string{"br_netfilter", "overlay", "nf_conntrack", "wireguard"}

	for _, mod := range required {
		loaded, err := sysModprobe(mod)
		if loaded {
			continue
		}

		// Could not get it loaded. Decide whether this is a confident block or
		// an "unknown tooling" situation. If modprobe is missing entirely we
		// cannot assert -> warn, do not block.
		if _, lookErr := exec.LookPath("modprobe"); lookErr != nil {
			roslog.W("modprobe not found; cannot verify kernel module load, skipping", nil, "module", mod)
			return nil
		}

		// Build the precise cause.
		var cause, fix string
		switch {
		case modulesTreeMissing:
			cause = fmt.Sprintf("the /lib/modules tree for kernel %s is missing (%s not found)", rel, modulesDep)
			fix = fmt.Sprintf("sudo apt-get install -y linux-modules-extra-%s (reboot if a newer kernel is pending), then re-run", rel)
		case modulesDisabled:
			cause = "kernel.modules_disabled=1 (module loading is permanently disabled until reboot)"
			fix = "reboot the host (or boot it without the hardening that sets kernel.modules_disabled), then re-run"
		case lockdown != "":
			cause = fmt.Sprintf("kernel lockdown is active (%s), typically from Secure Boot", lockdown)
			fix = "disable Secure Boot/lockdown for this host, or pre-load the modules via /etc/modules-load.d/, then re-run"
		default:
			if bl := sysModuleBlacklisted(mod); bl != "" {
				cause = fmt.Sprintf("it is blacklisted in %s", bl)
				fix = fmt.Sprintf("remove the 'blacklist %s' line from %s, run 'sudo depmod -a', then re-run", mod, bl)
			} else if err != nil {
				cause = fmt.Sprintf("modprobe failed: %v", err)
				fix = fmt.Sprintf("sudo apt-get install -y linux-modules-extra-%s, ensure the module exists, then re-run", rel)
			} else {
				// modprobe returned no error yet the module is not visible: too
				// ambiguous to block on.
				roslog.W("module not visible after modprobe but no error reported; not blocking", nil, "module", mod)
				continue
			}
		}

		return fmt.Errorf("required kernel module %q could not actually be loaded (a dry-run can pass while the real load fails). Cause: %s.\n\nFix: %s. Host prerequisite, not a RunOS error.", mod, cause, fix)
	}

	return nil
}

// checkRequiredSysctlsWritable verifies the kernel actually accepts the sysctl
// writes Kubernetes requires (net.ipv4.ip_forward and, after loading
// br_netfilter, net.bridge.bridge-nf-call-iptables) via a no-op write-back, and
// detects a read-only /proc/sys or a conflicting '=0' drop-in that would defeat
// the install. Prevents the silent "pods have no network / NodePorts dead"
// failure on hardened hosts and unprivileged containers.
func checkRequiredSysctlsWritable() error {
	// Hard signal: /proc/sys mounted read-only.
	if ro, ok := sysProcSysReadOnly(); ok && ro {
		return fmt.Errorf("/proc/sys is mounted read-only, so the kernel refuses the sysctl writes Kubernetes requires (net.ipv4.ip_forward, net.bridge.bridge-nf-call-iptables). This is common on hardened hosts and unprivileged containers.\n\nProvision a normal VM/bare-metal host where 'sysctl -w' succeeds, or remove the read-only protection on /proc/sys, then re-run. RunOS cannot configure pod networking without it.")
	}

	// ip_forward must be writable.
	if writable, werr, ok := sysSysctlWritable("net.ipv4.ip_forward"); ok && !writable && sysErrIsPerm(werr) {
		return fmt.Errorf("the kernel refuses the write to net.ipv4.ip_forward (%v), which Kubernetes requires for pod routing. /proc/sys appears write-protected (common on hardened hosts and unprivileged containers).\n\nProvision a normal VM/bare-metal host where 'sysctl -w net.ipv4.ip_forward=1' succeeds, or remove the write protection, then re-run. RunOS cannot configure pod networking without it.", werr)
	}

	// bridge-nf-call-iptables requires br_netfilter to exist first. Load it (best
	// effort) and then test writability. Treat a missing module as non-blocking
	// here (the module check owns that verdict).
	sysModprobe("br_netfilter")
	if sysFileExists(sysSysctlPath("net.bridge.bridge-nf-call-iptables")) {
		if writable, werr, ok := sysSysctlWritable("net.bridge.bridge-nf-call-iptables"); ok && !writable && sysErrIsPerm(werr) {
			return fmt.Errorf("the kernel refuses the write to net.bridge.bridge-nf-call-iptables (%v), which Kubernetes requires so bridged traffic traverses iptables. /proc/sys appears write-protected.\n\nProvision a normal VM/bare-metal host where this sysctl is writable, or remove the write protection, then re-run. RunOS cannot configure pod networking without it.", werr)
		}
	}

	// Soft note: a conflicting '=0' override in sysctl.d would be re-applied at
	// boot and defeat the installer. Warn, do not block (the installer also sets
	// these explicitly).
	conf := sysGlobReadAll(
		"/etc/sysctl.d/*.conf", "/run/sysctl.d/*.conf", "/usr/lib/sysctl.d/*.conf", "/etc/sysctl.conf",
	)
	for _, key := range []string{"net.ipv4.ip_forward", "net.bridge.bridge-nf-call-iptables"} {
		if sysSysctlOverriddenZero(conf, key) {
			roslog.W("a sysctl drop-in sets a required key to 0; the installer overrides it but a reboot may re-disable it", nil, "key", key)
		}
	}

	return nil
}

// sysSysctlOverriddenZero reports whether the concatenated sysctl.d content sets
// key to 0 (ignoring comments/whitespace).
func sysSysctlOverriddenZero(content, key string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		if strings.TrimSpace(parts[0]) == key && strings.TrimSpace(parts[1]) == "0" {
			return true
		}
	}
	return false
}

// checkLinuxCapabilities verifies the process (root) actually holds the Linux
// capabilities RunOS needs (CAP_SYS_ADMIN, CAP_NET_ADMIN, CAP_SYS_MODULE,
// CAP_NET_RAW). euid==0 with capabilities dropped (a confined systemd unit or
// restricted container) cannot manage interfaces, mounts or modules. Prevents
// the confusing "running as root but EPERM everywhere" failure. SELinux
// enforcing / seccomp filtered are surfaced as soft notes only.
func checkLinuxCapabilities() error {
	capEff, ok := sysReadCapField("CapEff")
	if !ok {
		// Cannot read CapEff: too ambiguous to block.
		roslog.W("could not read CapEff from /proc/self/status; skipping capability check", nil)
		return nil
	}

	needed := []struct {
		bit  uint
		name string
	}{
		{sysCapSysAdmin, "CAP_SYS_ADMIN"},
		{sysCapNetAdmin, "CAP_NET_ADMIN"},
		{sysCapSysModule, "CAP_SYS_MODULE"},
		{sysCapNetRaw, "CAP_NET_RAW"},
	}
	var missing []string
	for _, c := range needed {
		if !sysHasCap(capEff, c.bit) {
			missing = append(missing, c.name)
		}
	}

	// Soft notes (warn only): seccomp mode 2 and SELinux enforcing can also
	// constrain runtimes, but plenty of working hosts run them, so never block.
	if seccomp, sok := sysReadCapStatusField("Seccomp"); sok && seccomp == "2" {
		roslog.W("a seccomp filter (mode 2) is active on this process; if container runtimes misbehave, check the seccomp profile", nil)
	}
	if v, eok := sysReadFileTrim("/sys/fs/selinux/enforce"); eok && v == "1" {
		roslog.W("SELinux is in enforcing mode; ensure containerd/kubelet are not confined by a custom policy", nil)
	}

	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("this process is root but key Linux capabilities are missing (need CAP_SYS_ADMIN, CAP_NET_ADMIN, CAP_SYS_MODULE, CAP_NET_RAW; missing: %s), so it cannot manage interfaces, mounts, or kernel modules. You appear to be inside a restricted/unprivileged container or a confined systemd unit.\n\nRun RunOS on a full VM or bare-metal host (or a fully --privileged unconfined container), then re-run. Environment prerequisite, not a RunOS error.", strings.Join(missing, ", "))
}

// sysReadCapStatusField reads an arbitrary single-valued field (first token)
// from /proc/self/status, e.g. "Seccomp".
func sysReadCapStatusField(field string) (string, bool) {
	data, ok := sysReadFileTrim("/proc/self/status")
	if !ok {
		return "", false
	}
	for _, line := range strings.Split(data, "\n") {
		if strings.HasPrefix(line, field+":") {
			val := strings.TrimSpace(strings.TrimPrefix(line, field+":"))
			if f := strings.Fields(val); len(f) > 0 {
				return f[0], true
			}
			return "", false
		}
	}
	return "", false
}
