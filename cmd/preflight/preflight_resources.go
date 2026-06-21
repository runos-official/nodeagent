package preflight

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// resMountInfo is a parsed /proc/mounts entry (the fields we care about).
type resMountInfo struct {
	source     string   // device / source
	mountPoint string   // where it is mounted
	fsType     string   // ext4, xfs, nfs, overlay, tmpfs, ...
	options    []string // mount options (rw, noexec, ro, ...)
}

// resReadMounts parses /proc/mounts. Returns nil (no entries) if it cannot be
// read so callers degrade to "cannot determine" rather than a false positive.
func resReadMounts() []resMountInfo {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []resMountInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		// /proc/mounts escapes spaces as \040 etc; unescape the mount point.
		out = append(out, resMountInfo{
			source:     fields[0],
			mountPoint: resUnescape(fields[1]),
			fsType:     fields[2],
			options:    strings.Split(fields[3], ","),
		})
	}
	return out
}

// resUnescape decodes octal escapes (\040 space, \011 tab, ...) used in
// /proc/mounts mount-point fields.
func resUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseInt(s[i+1:i+4], 8, 16); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// resMountFor returns the /proc/mounts entry whose mount point is the longest
// prefix of path (i.e. the filesystem that actually backs path). ok=false if
// nothing matched (then the caller must not assert anything).
func resMountFor(path string, mounts []resMountInfo) (resMountInfo, bool) {
	clean := filepath.Clean(path)
	best := -1
	var bestLen int
	for i, m := range mounts {
		mp := m.mountPoint
		if mp == clean || mp == "/" || strings.HasPrefix(clean, strings.TrimRight(mp, "/")+"/") {
			if len(mp) >= bestLen {
				bestLen = len(mp)
				best = i
			}
		}
	}
	if best < 0 {
		return resMountInfo{}, false
	}
	return mounts[best], true
}

// resNearestExisting walks up from path to the nearest existing ancestor, so we
// can statfs/resolve a data dir that does not exist yet (e.g. /var/lib/etcd
// before kubeadm runs). Always terminates at "/".
func resNearestExisting(path string) string {
	p := filepath.Clean(path)
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "/"
		}
		p = parent
	}
}

// resStatfs wraps unix.Statfs and returns ok=false on error so a missing path
// never produces a false finding.
func resStatfs(path string) (unix.Statfs_t, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return st, false
	}
	return st, true
}

// resFreeBytes returns available bytes for non-root from a statfs result.
func resFreeBytes(st unix.Statfs_t) uint64 {
	return st.Bavail * uint64(st.Bsize)
}

// resHasOption reports whether a mount option list contains opt.
func resHasOption(opts []string, opt string) bool {
	for _, o := range opts {
		if o == opt {
			return true
		}
	}
	return false
}

const resGiB = 1024 * 1024 * 1024

// checkVarMountSpaceAndInodes statfs's the REAL filesystem backing each data
// directory Kubernetes writes to (containerd images, etcd, apt archives, /tmp),
// not just "/", and blocks if any is short on space or inodes. Prevents the
// cryptic mid-install failure where "df /" looks healthy but a separate /var (or
// /var/lib) volume is tiny, so image pulls / etcd writes fail with ENOSPC or
// "no space left on device" that never mentions inodes.
func checkVarMountSpaceAndInodes() error {
	mounts := resReadMounts()

	// dir -> minimum required free GiB. /var/lib backs containerd + etcd state.
	type req struct {
		path  string
		minGB float64
		label string
	}
	reqs := []req{
		{"/var/lib/containerd", 15, "containerd images"},
		{"/var/lib/etcd", 8, "etcd data"},
		{"/var/cache/apt/archives", 2, "apt archives"},
		{"/tmp", 2, "/tmp"},
	}

	// Deduplicate probes by backing device so we report each filesystem once,
	// against the strictest requirement that lands on it.
	type devReq struct {
		statPath string
		minGB    float64
		labels   []string
		key      string
	}
	byKey := map[string]*devReq{}
	var order []string
	for _, r := range reqs {
		statPath := resNearestExisting(r.path)
		if _, ok := resStatfs(statPath); !ok {
			continue
		}
		// Key on the backing device if we can identify it, else the stat path.
		key := statPath
		if m, found := resMountFor(statPath, mounts); found {
			key = m.mountPoint + "\x00" + m.source
		}
		if d, exists := byKey[key]; exists {
			if r.minGB > d.minGB {
				d.minGB = r.minGB
			}
			d.labels = append(d.labels, r.label)
		} else {
			byKey[key] = &devReq{statPath: statPath, minGB: r.minGB, labels: []string{r.label}, key: key}
			order = append(order, key)
		}
	}

	var problems []string
	for _, key := range order {
		d := byKey[key]
		st, ok := resStatfs(d.statPath)
		if !ok {
			continue
		}
		freeBytes := resFreeBytes(st)
		freeGB := float64(freeBytes) / float64(resGiB)
		if freeGB < d.minGB {
			problems = append(problems, fmt.Sprintf(
				"%s (backs %s): %.1f GB free, need >= %.0f GB",
				d.statPath, strings.Join(d.labels, " + "), freeGB, d.minGB))
		}

		// Inode check: block if free inodes < 5% of total or < 200k absolute.
		// Files==0 means the FS does not report inodes (e.g. some btrfs/tmpfs);
		// skip the inode assertion in that case rather than false-positive.
		if st.Files > 0 {
			fivePct := st.Files / 20
			if st.Ffree < 200000 && st.Ffree < fivePct {
				problems = append(problems, fmt.Sprintf(
					"%s (backs %s): only %d free inodes (%.1f%% of %d) — will exhaust before bytes do",
					d.statPath, strings.Join(d.labels, " + "),
					st.Ffree, 100*float64(st.Ffree)/float64(st.Files), st.Files))
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}

	return fmt.Errorf(
		"a filesystem Kubernetes writes to is too small or out of inodes (even though / may look fine):\n  - %s\n\n"+
			"Container images and etcd fill /var fast. Grow the /var (or /var/lib) volume to 20 GB+ free\n"+
			"and free inodes, or move it to a larger disk, then re-run:\n"+
			"  df -h /var /var/lib /tmp\n"+
			"  df -i /var /var/lib /tmp\n"+
			"This is an environment requirement, not a RunOS error.",
		strings.Join(problems, "\n  - "))
}

// checkTmpExecAndDataMounts blocks when a filesystem in the install/runtime
// path is mounted noexec or read-only. Prevents the baffling failure on
// CIS-hardened / immutable images where RunOS cannot exec helper binaries
// (kubeadm, helm, CNI plugins) or write runtime state EVEN AS ROOT, surfacing
// as a bare "permission denied" on an executable that is clearly +x and owned
// by root. Uses a real write+exec probe for /tmp (definitive) plus mount flags.
func checkTmpExecAndDataMounts() error {
	mounts := resReadMounts()
	var problems []string

	// 1. /tmp (and TMPDIR if different): definitive write+exec probe.
	tmpDirs := []string{"/tmp"}
	if td := strings.TrimSpace(os.Getenv("TMPDIR")); td != "" && filepath.Clean(td) != "/tmp" {
		tmpDirs = append(tmpDirs, filepath.Clean(td))
	}
	for _, dir := range tmpDirs {
		if noexec, definitive := resTmpNoexec(dir); noexec && definitive {
			problems = append(problems, fmt.Sprintf("%s is noexec (a 0700 script there cannot be executed)", dir))
		} else if !definitive {
			// Could not run the probe (dir missing, write failed for another
			// reason): fall back to the mount flag, never hard-assert blindly.
			if m, ok := resMountFor(dir, mounts); ok && resHasOption(m.options, "noexec") {
				problems = append(problems, fmt.Sprintf("%s mount has 'noexec' (per /proc/mounts)", dir))
			}
		}
	}

	// 2. /var/lib and /opt (incl. /opt/cni/bin): must be read-write and exec.
	for _, dir := range []string{"/var/lib", "/opt"} {
		statPath := resNearestExisting(dir)
		m, ok := resMountFor(statPath, mounts)
		if !ok {
			continue
		}
		if resHasOption(m.options, "ro") {
			problems = append(problems, fmt.Sprintf("%s is mounted read-only (%s)", dir, m.mountPoint))
		}
		if resHasOption(m.options, "noexec") {
			problems = append(problems, fmt.Sprintf("%s is mounted noexec (%s)", dir, m.mountPoint))
		}
	}

	if len(problems) == 0 {
		return nil
	}

	return fmt.Errorf(
		"a filesystem in the install/runtime path is mounted 'noexec' or read-only:\n  - %s\n\n"+
			"RunOS cannot execute helper binaries (kubeadm/helm/CNI) or write runtime state, even as root\n"+
			"(noexec is enforced for root too). Fix it, then re-run:\n"+
			"  sudo mount -o remount,exec,rw /tmp   # and the affected mount\n"+
			"  # make it durable by fixing the entry in /etc/fstab\n"+
			"  # or export an exec-capable TMPDIR before running the installer\n"+
			"CIS-hardened / immutable images commonly set these flags.",
		strings.Join(problems, "\n  - "))
}

// resTmpNoexec writes a tiny 0700 script into dir and executes it. Returns
// (noexec, definitive): definitive=false means the probe could not run (so the
// caller must not conclude anything from it). noexec=true only when exec failed
// specifically because the filesystem refuses execution.
func resTmpNoexec(dir string) (noexec bool, definitive bool) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return false, false
	}
	f, err := os.CreateTemp(dir, "runos-preflight-*.sh")
	if err != nil {
		return false, false
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.WriteString("#!/bin/sh\nexit 0\n"); err != nil {
		f.Close()
		return false, false
	}
	if err := f.Close(); err != nil {
		return false, false
	}
	if err := os.Chmod(name, 0o700); err != nil {
		return false, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = exec.CommandContext(ctx, name).Run()
	if err == nil {
		return false, true // executed fine -> exec allowed
	}
	// EACCES / "permission denied" on a 0700 root-owned file we just wrote is
	// the noexec signature. Anything else (e.g. no /bin/sh) is inconclusive.
	if strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		return true, true
	}
	return false, false
}

// checkSwapPersistence blocks when swap is active OR will be re-enabled on
// reboot, going beyond the live /proc/swaps view (checkSwap). Catches the trap
// where 'swapoff -a' succeeds now but a swap line in /etc/fstab, a systemd swap
// unit, or zram brings swap back after the next reboot, after which the kubelet
// silently degrades because Kubernetes requires swap permanently off.
func checkSwapPersistence() error {
	var reasons []string

	// 1. Uncommented swap line in /etc/fstab (survives reboot).
	if data, err := os.ReadFile("/etc/fstab"); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[2] == "swap" {
				reasons = append(reasons, fmt.Sprintf("uncommented swap line in /etc/fstab: %q", line))
			}
		}
	}

	// 2. Active systemd swap units (covers fstab-generated + explicit .swap).
	if units := resActiveSwapUnits(); len(units) > 0 {
		reasons = append(reasons, "active swap unit(s): "+strings.Join(units, ", "))
	}

	// 3. zram-backed swap (zram-generator). Detect a zram* device in /proc/swaps
	//    and/or an active systemd-zram-setup@ unit.
	if zram := resZramSwap(); len(zram) > 0 {
		reasons = append(reasons, "zram swap: "+strings.Join(zram, ", "))
	}

	if len(reasons) == 0 {
		return nil
	}

	return fmt.Errorf(
		"swap is active or will be re-enabled on reboot:\n  - %s\n\n"+
			"Kubernetes requires swap permanently off; a plain 'swapoff -a' will not stick. Disable it durably:\n"+
			"  sudo swapoff -a\n"+
			"  sudo sed -i '/\\sswap\\s/ s/^/#/' /etc/fstab\n"+
			"  # for zram: sudo systemctl disable --now 'systemd-zram-setup@zram0' (or remove the zram-generator package)\n"+
			"Then re-run.",
		strings.Join(reasons, "\n  - "))
}

// resActiveSwapUnits lists active systemd swap units via systemctl. Returns
// empty on any error (systemd absent, timeout) so it never false-positives.
func resActiveSwapUnits() []string {
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "list-units", "--type=swap", "--state=active", "--no-legend", "--plain", "--no-pager").Output()
	if err != nil {
		return nil
	}
	var units []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 0 && strings.HasSuffix(fields[0], ".swap") {
			units = append(units, fields[0])
		}
	}
	return units
}

// resZramSwap reports zram-backed swap: zram* devices in /proc/swaps and active
// systemd-zram-setup@ units. Empty on any uncertainty.
func resZramSwap() []string {
	var found []string
	if data, err := os.ReadFile("/proc/swaps"); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		first := true
		for sc.Scan() {
			if first { // header
				first = false
				continue
			}
			fields := strings.Fields(sc.Text())
			if len(fields) > 0 && strings.Contains(fields[0], "zram") {
				found = append(found, fields[0]+" (in /proc/swaps)")
			}
		}
	}
	if path, err := exec.LookPath("systemctl"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, path, "list-units", "systemd-zram-setup@*", "--state=active", "--no-legend", "--plain", "--no-pager").Output()
		if err == nil {
			sc := bufio.NewScanner(strings.NewReader(string(out)))
			for sc.Scan() {
				fields := strings.Fields(sc.Text())
				if len(fields) > 0 && strings.HasPrefix(fields[0], "systemd-zram-setup@") {
					found = append(found, fields[0]+" (active unit)")
				}
			}
		}
	}
	return found
}

// checkRamAvailableAndPressure is an advisory that corroborates total RAM (the
// hard floor lives in checkRAM) with the live picture: MemAvailable, PSI memory
// pressure, and recent OOM kills. Warns when the node passes the size gate but
// is so memory-starved that kubeadm/kubelet will be OOM-killed mid-bootstrap,
// which otherwise looks like a random hang or a kubelet that restarts forever.
func checkRamAvailableAndPressure() error {
	meminfo := resReadMeminfo()
	memTotalKB, okTotal := meminfo["MemTotal"]
	memAvailKB, okAvail := meminfo["MemAvailable"]
	if !okTotal {
		// Cannot read /proc/meminfo reliably; checkRAM owns the hard path.
		return nil
	}

	var notes []string

	// WARN band: total RAM below RunOS recommended (3.5GB) but above kubeadm's
	// real floor (~1.7GB). Below 1.7GB is the BLOCK case owned by checkRAM.
	totalGB := float64(memTotalKB) / 1024 / 1024
	if totalGB >= 1.7 && totalGB < 3.5 {
		notes = append(notes, fmt.Sprintf("%.1f GB total RAM is below the RunOS recommended 3.5 GB", totalGB))
	}

	// Low MemAvailable is advisory only (transient): warn under 1.5GB.
	if okAvail {
		availGB := float64(memAvailKB) / 1024 / 1024
		if availGB < 1.5 {
			notes = append(notes, fmt.Sprintf("only %.1f GB currently available (rest in use by other processes); bootstrap needs ~1.5 GB free", availGB))
		}
	}

	// Corroborate with PSI: sustained 'full avg10' > 10% means real stalls.
	if p := resMemoryPSIFullAvg10(); p > 10 {
		notes = append(notes, fmt.Sprintf("memory pressure stalls detected (PSI full avg10 = %.1f%%)", p))
	}

	// Recent OOM kills in the kernel ring buffer.
	if resRecentOOMKill() {
		notes = append(notes, "recent 'Out of memory: Killed process' entries in dmesg")
	}

	if len(notes) == 0 {
		return nil
	}

	return fmt.Errorf(
		"this node may be memory-starved for the Kubernetes bootstrap:\n  - %s\n\n"+
			"kubeadm/kubelet need ~1.5 GB free and will be OOM-killed otherwise. Free memory or use a dedicated node:\n"+
			"  ps aux --sort=-%%mem | head\n"+
			"  systemctl stop <heavy-service>\n"+
			"then re-run. Warning only.",
		strings.Join(notes, "\n  - "))
}

// resReadMeminfo parses /proc/meminfo into a key -> kB map. Empty on read error.
func resReadMeminfo() map[string]uint64 {
	out := map[string]uint64{}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
			out[key] = v // value is in kB
		}
	}
	return out
}

// resMemoryPSIFullAvg10 returns the 'full avg10' percentage from
// /proc/pressure/memory, or -1 if PSI is unavailable/unparseable.
func resMemoryPSIFullAvg10() float64 {
	data, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return -1
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "full ") {
			continue
		}
		for _, fld := range strings.Fields(line) {
			if strings.HasPrefix(fld, "avg10=") {
				if v, err := strconv.ParseFloat(strings.TrimPrefix(fld, "avg10="), 64); err == nil {
					return v
				}
			}
		}
	}
	return -1
}

// resRecentOOMKill greps the kernel ring buffer for OOM-killer activity. Returns
// false on any error (dmesg restricted, absent) so it never false-positives.
func resRecentOOMKill() bool {
	path, err := exec.LookPath("dmesg")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--ctime").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "Out of memory: Killed process") ||
		strings.Contains(string(out), "oom-kill:")
}

// checkEntropyAvailable warns (only) when the kernel entropy pool is low AND no
// hardware RNG is present, the combination under which key generation (mTLS,
// WireGuard, cluster certs) can block on getrandom for minutes. Prevents the
// confusing symptom of registration appearing to hang with no error. Warn-only
// because modern (5.6+) kernels rarely block on getrandom.
func checkEntropyAvailable() error {
	data, err := os.ReadFile("/proc/sys/kernel/random/entropy_avail")
	if err != nil {
		// Cannot read entropy; do not warn on a guess.
		return nil
	}
	e, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return nil
	}
	if e >= 256 {
		return nil
	}
	// Low entropy: only warn if there is no hardware RNG and no entropy daemon.
	if resHasHardwareRNG() || resEntropyDaemonActive() {
		return nil
	}
	return fmt.Errorf(
		"kernel entropy is low (entropy_avail=%d) and no hardware RNG was detected\n\n"+
			"Key generation (mTLS, WireGuard, cluster certs) may stall for minutes on this host.\n"+
			"Add a virtio-rng device to the VM, or feed the pool:\n"+
			"  sudo apt-get install -y rng-tools5   # or: haveged\n"+
			"then re-run if registration hangs. Warning only.", e)
}

// resHasHardwareRNG reports whether a hardware RNG source is present.
func resHasHardwareRNG() bool {
	if _, err := os.Stat("/dev/hwrng"); err == nil {
		return true
	}
	if data, err := os.ReadFile("/sys/class/misc/hw_random/rng_current"); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" && s != "none" {
			return true
		}
	}
	return false
}

// resEntropyDaemonActive reports whether haveged or rng-tools is active.
func resEntropyDaemonActive() bool {
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return false
	}
	for _, svc := range []string{"haveged", "rng-tools", "rngd", "rng-tools-debian"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, _ := exec.CommandContext(ctx, path, "is-active", svc).Output()
		cancel()
		if strings.TrimSpace(string(out)) == "active" {
			return true
		}
	}
	return false
}

// checkEtcdDiskFsyncLatency warns (only) when the disk backing /var/lib/etcd is
// too slow for etcd: it micro-benchmarks fsync latency and flags network
// filesystems. Prevents the hard-to-diagnose control-plane instability where
// etcd leader elections flap and kubeadm fails to bootstrap because the data
// disk (HDD, throttled burst volume, or NFS) cannot meet etcd's <10ms fsync
// target. Warn-only due to benchmark variance.
func checkEtcdDiskFsyncLatency() error {
	dir := resNearestExisting("/var/lib/etcd")

	// 1. Network/overlay filesystem is an immediate (non-benchmark) red flag.
	if m, ok := resMountFor(dir, resReadMounts()); ok {
		switch {
		case strings.HasPrefix(m.fsType, "nfs"), m.fsType == "cifs", m.fsType == "smb",
			m.fsType == "smbfs", m.fsType == "fuse.glusterfs", m.fsType == "ceph":
			return fmt.Errorf(
				"the disk backing /var/lib/etcd is a network filesystem (%s at %s)\n\n"+
					"etcd will be unstable: leader elections flap and the control plane may fail to bootstrap.\n"+
					"Use a LOCAL SSD/NVMe (not NFS/CIFS/Gluster/Ceph) for the etcd data directory, then re-run.\n"+
					"Storage-performance prerequisite, not a RunOS error.", m.fsType, m.mountPoint)
		}
	}

	// 2. fsync micro-benchmark: ~100 x 4KB writes with fsync, measure p99.
	p99, ok := resFsyncP99(dir, 100)
	if !ok {
		// Could not benchmark (e.g. read-only / no write perm): the dedicated
		// mount-flag check owns that; don't warn on a guess here.
		return nil
	}
	if p99 <= 10*time.Millisecond {
		return nil
	}
	return fmt.Errorf(
		"the disk backing /var/lib/etcd is slow (measured fsync p99 ~%.0f ms; etcd needs < 10 ms)\n\n"+
			"etcd will be unstable: leader elections flap and the control plane may fail to bootstrap.\n"+
			"Use a local SSD/NVMe (not an HDD, a throttled burst volume, or NFS) for the etcd data directory,\n"+
			"then re-run. Storage-performance prerequisite, not a RunOS error.",
		float64(p99.Microseconds())/1000.0)
}

// resFsyncP99 writes n 4KB blocks to a temp file under dir, fsync'ing each, and
// returns the p99 latency. ok=false if the benchmark could not run.
func resFsyncP99(dir string, n int) (time.Duration, bool) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return 0, false
	}
	f, err := os.CreateTemp(dir, ".runos-fsync-*.tmp")
	if err != nil {
		return 0, false
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()

	buf := make([]byte, 4096)
	lats := make([]time.Duration, 0, n)
	deadline := time.Now().Add(6 * time.Second)
	for i := 0; i < n; i++ {
		if time.Now().After(deadline) {
			break // bound total time; use whatever samples we have
		}
		start := time.Now()
		if _, err := f.WriteAt(buf, 0); err != nil {
			return 0, false
		}
		if err := f.Sync(); err != nil {
			return 0, false
		}
		lats = append(lats, time.Since(start))
	}
	if len(lats) < 10 {
		return 0, false // too few samples to trust a p99
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	idx := (len(lats) * 99) / 100
	if idx >= len(lats) {
		idx = len(lats) - 1
	}
	return lats[idx], true
}

// checkResourceLimits warns (only) when process / file-watch / memory limits on
// this host are low enough to bite Kubernetes under load. Prevents the family of
// misleading errors ("Resource temporarily unavailable", "too many open files",
// "inotify: no space left on device", "cannot allocate memory") that stem from
// kernel.pid_max, cgroup pids.max, fs.inotify watches/instances, the nofile
// ulimit, or vm.overcommit_memory=2 rather than from anything RunOS did.
func checkResourceLimits() error {
	var notes []string

	if v, ok := resReadIntFile("/proc/sys/kernel/pid_max"); ok && v < 32768 {
		notes = append(notes, fmt.Sprintf("kernel.pid_max=%d (recommend >= 32768)", v))
	}

	// Root cgroup pids.max (cgroup v2). "max" means unlimited (fine).
	if data, err := os.ReadFile("/sys/fs/cgroup/pids.max"); err == nil {
		s := strings.TrimSpace(string(data))
		if s != "" && s != "max" {
			if v, err := strconv.Atoi(s); err == nil && v < 4096 {
				notes = append(notes, fmt.Sprintf("root cgroup pids.max=%d is constrained", v))
			}
		}
	}

	// Only warn on genuinely crippled values — Ubuntu defaults (watches ~60k,
	// instances 128, nofile soft 1024) are fine for a normal cluster, and the
	// kubelet/containerd systemd units raise their own NOFILE; warning on stock
	// defaults is noise that makes a healthy node look broken.
	if v, ok := resReadIntFile("/proc/sys/fs/inotify/max_user_watches"); ok && v < 8192 {
		notes = append(notes, fmt.Sprintf("fs.inotify.max_user_watches=%d is very low (recommend >= 524288)", v))
	}
	if v, ok := resReadIntFile("/proc/sys/fs/inotify/max_user_instances"); ok && v < 128 {
		notes = append(notes, fmt.Sprintf("fs.inotify.max_user_instances=%d is below the default (recommend >= 512)", v))
	}

	// RLIMIT_NOFILE soft limit for this (root) process.
	var nofile unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &nofile); err == nil {
		if nofile.Cur < 1024 {
			notes = append(notes, fmt.Sprintf("nofile (RLIMIT_NOFILE) soft limit=%d (recommend >= 65536)", nofile.Cur))
		}
	}

	if v, ok := resReadIntFile("/proc/sys/vm/overcommit_memory"); ok && v == 2 {
		notes = append(notes, "vm.overcommit_memory=2 (strict; can cause 'cannot allocate memory')")
	}

	if len(notes) == 0 {
		return nil
	}

	return fmt.Errorf(
		"process / file-watch / memory limits on this host are low:\n  - %s\n\n"+
			"Kubernetes components spawn many processes, open many fds, and watch many files; under load you may\n"+
			"see 'Resource temporarily unavailable', 'too many open files', a misleading\n"+
			"'inotify: no space left on device', or 'cannot allocate memory'. Raise them:\n"+
			"  sudo sysctl -w kernel.pid_max=4194304\n"+
			"  sudo sysctl -w fs.inotify.max_user_watches=524288\n"+
			"  sudo sysctl -w fs.inotify.max_user_instances=512\n"+
			"  sudo sysctl -w vm.overcommit_memory=0\n"+
			"  # raise the nofile ulimit (e.g. LimitNOFILE in the service unit / /etc/security/limits.conf)\n"+
			"then re-run. Warning only.",
		strings.Join(notes, "\n  - "))
}

// resReadIntFile reads a single-integer /proc or /sys file. ok=false on any
// error or non-integer content so callers never assert on garbage.
func resReadIntFile(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return v, true
}
