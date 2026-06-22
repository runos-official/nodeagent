package preflight

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/cobra"
)

// server is the resolved Nodeward host the install will register against. The
// installer passes it via `runos preflight --server <host>` so we can probe the
// one host registration actually needs (DNS/firewall classification). Empty is
// allowed: the nodeward probe is skipped with a warning rather than failing.
var server string

// cdnURL is the base CDN URL the installer pulls artifacts (the L1Sec CA, etc.)
// from, passed via `runos preflight --cdn <url>`. Empty -> CDN reachability is
// probed against a default/derived host or skipped with a warning.
var cdnURL string

var RootCmd = &cobra.Command{
	Use:   "preflight",
	Short: "Check if the system is ready for installation",
	Long:  `Performs pre-flight checks to ensure the system is ready for node registration and installation`,
	Run: func(cmd *cobra.Command, args []string) {
		if err := runPreflightChecks(); err != nil {
			// runPreflightChecks has already printed the detailed findings and the
			// support line; just exit non-zero.
			os.Exit(1)
		}
		fmt.Println("\nSystem is ready for installation.")
	},
}

func init() {
	RootCmd.PersistentFlags().StringVarP(&server, "server", "s", "",
		"Nodeward host this node will register against (probed for reachability)")
	RootCmd.PersistentFlags().StringVar(&cdnURL, "cdn", "",
		"Base CDN URL the installer pulls artifacts from (probed for reachability)")
}

// checkRoot ensures we are running as the effective root user. Almost every
// downstream step (config writes to /etc/runos, modprobe, apt, kubeadm) needs
// it; without this the failure surfaces much later as an opaque EACCES.
func checkRoot() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root (effective UID 0)\n\nRe-run with sudo, e.g.: sudo curl -sSL <url> | sudo bash\nor: sudo runos preflight")
	}
	return nil
}

// checkArch ensures the CPU architecture matches a binary we publish. The
// installer shell only maps x86_64/aarch64; anything else cannot run the agent.
func checkArch() error {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return fmt.Errorf("unsupported CPU architecture %q: only amd64 and arm64 are supported\n\nProvision the node on an amd64 or arm64 host and re-run", runtime.GOARCH)
	}
	return nil
}

// checkSwap fails if swap is enabled. kubeadm hard-rejects swap, and the worker
// join path masks it with --ignore-preflight-errors=all, producing a kubelet
// that crash-loops instead of a clean error. Catch it up front.
func checkSwap() error {
	data, err := os.ReadFile("/proc/swaps")
	if err != nil {
		// No /proc/swaps (unusual) -> nothing we can assert; don't block.
		return nil
	}
	// /proc/swaps always has a header line; a second non-empty line means an
	// active swap area.
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 1 {
		return fmt.Errorf("swap is enabled; Kubernetes/kubeadm require swap off\n\nDisable it now with:\n  sudo swapoff -a\n  sudo sed -i '/ swap / s/^/#/' /etc/fstab\nThen re-run the installer")
	}
	return nil
}

// checkPortsFree fails if any kubeadm/RunOS-required port is already bound. A
// prior partial install, a stray etcd/haproxy, or another k8s leaves these
// taken and the relevant component later fails with a port-specific error that
// doesn't name the owning process.
func checkPortsFree() error {
	tcpPorts := map[int]string{
		6443:  "kube-apiserver",
		10250: "kubelet",
		2379:  "etcd",
		2380:  "etcd-peer",
		6446:  "haproxy(runos)",
	}
	// 51820 (wireguard) and 8472 (cilium vxlan) are UDP.
	udpPorts := map[int]string{
		51820: "wireguard",
		8472:  "cilium-vxlan",
	}

	var taken []string
	for p, name := range tcpPorts {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			taken = append(taken, fmt.Sprintf("%d/tcp (%s)", p, name))
			continue
		}
		ln.Close()
	}
	for p, name := range udpPorts {
		pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", p))
		if err != nil {
			taken = append(taken, fmt.Sprintf("%d/udp (%s)", p, name))
			continue
		}
		pc.Close()
	}

	if len(taken) > 0 {
		return fmt.Errorf("required ports already in use: %s\n\nFind the owner with: sudo ss -tulpnH 'sport = :<port>'\nStop the conflicting service, or run 'sudo kubeadm reset -f' if this is a stale install, then re-run", strings.Join(taken, ", "))
	}
	return nil
}

// checkClockSkew warns/fails when the system clock is not NTP-synchronized. A
// skewed clock breaks the mTLS handshake to nodeward ("certificate is not yet
// valid / has expired") and apt Release-file validation, both of which surface
// as cryptic TLS/apt errors with no hint that the clock is the cause.
func checkClockSkew() error {
	// timedatectl is the canonical source on Ubuntu. If it's absent we can't
	// reliably assert sync, so we skip rather than block.
	path, err := exec.LookPath("timedatectl")
	if err != nil {
		roslog.W("cannot verify clock sync: timedatectl not found; skipping clock check", nil)
		return nil
	}
	out, err := exec.Command(path, "show", "-p", "NTPSynchronized", "--value").Output()
	if err != nil {
		// timedatectl present but errored (e.g. no systemd) -> warn, don't block.
		roslog.W("could not query clock sync state; skipping clock check", err)
		return nil
	}
	if strings.TrimSpace(string(out)) == "yes" {
		return nil
	}
	// Not synced yet. A freshly-provisioned cloud box often just hasn't synced its
	// clock; blocking here would break automated provisioning (the operator never
	// sees this, and there's nobody to run the fix). So self-heal: enable NTP and
	// wait briefly for sync, then re-check. Only fail if the clock genuinely refuses
	// to sync (e.g. NTP egress on UDP 123 is blocked) -- a real environment problem.
	roslog.I("clock not NTP-synchronized; enabling NTP (timedatectl set-ntp true) and waiting up to 45s for sync")
	_ = exec.Command(path, "set-ntp", "true").Run()
	for i := 0; i < 15; i++ {
		time.Sleep(3 * time.Second)
		if out, err = exec.Command(path, "show", "-p", "NTPSynchronized", "--value").Output(); err == nil && strings.TrimSpace(string(out)) == "yes" {
			roslog.I("clock is now NTP-synchronized")
			return nil
		}
	}
	return fmt.Errorf("system clock is not NTP-synchronized and did not sync after enabling NTP and waiting ~45s; TLS handshakes to Nodeward and package mirrors may fail with 'certificate not yet valid/expired'\n\nThe installer already ran 'timedatectl set-ntp true' -- check NTP egress (UDP 123 outbound) and 'timedatectl', then re-run")
}

// checkNodewardReachable dials the configured Nodeward host on its registration
// (9191) and operations (9192) ports, classifying DNS failure vs refused vs
// timeout/firewall so the operator knows whether it's --server, DNS, or egress.
// Skipped (with a warning) when --server was not provided.
func checkNodewardReachable() error {
	host := strings.TrimSpace(server)
	if host == "" {
		roslog.W("preflight ran without --server; skipping Nodeward reachability probe", nil)
		return nil
	}

	for _, p := range []struct {
		port int
		name string
	}{
		{9191, "registration (L1Sec)"},
		{9192, "operations (L2Sec)"},
	} {
		addr := net.JoinHostPort(host, strconv.Itoa(p.port))
		conn, err := net.DialTimeout("tcp", addr, 8*time.Second)
		if err != nil {
			return fmt.Errorf("cannot reach Nodeward %s at %s: %s\n\nVerify with: nc -vz %s %d", p.name, addr, classifyDialError(err, host), host, p.port)
		}
		conn.Close()
	}
	return nil
}

// classifyDialError turns a net.Dial error into an operator-facing cause +
// remedy: DNS failure, connection refused, or timeout/firewall.
func classifyDialError(err error, host string) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no such host") || strings.Contains(msg, "server misbehaving") || strings.Contains(msg, "name resolution"):
		return fmt.Sprintf("DNS resolution failed for %q — check /etc/resolv.conf and that --server is the correct host", host)
	case strings.Contains(msg, "connection refused"):
		return "connection refused — the host is reachable but nothing is listening on that port (wrong port, or Nodeward is down)"
	case strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "deadline exceeded"):
		return "connection timed out — an egress firewall is likely blocking this TCP port, or there is no route"
	default:
		return msg
	}
}

// checkRebootRequired checks if the system requires a reboot
func checkRebootRequired() error {
	rebootRequiredFile := "/var/run/reboot-required"

	if _, err := os.Stat(rebootRequiredFile); err == nil {
		// File exists, reboot is required

		// Try to read the packages that require reboot
		packagesFile := "/var/run/reboot-required.pkgs"
		var pkgInfo string
		if content, err := os.ReadFile(packagesFile); err == nil {
			pkgs := strings.TrimSpace(string(content))
			if pkgs != "" {
				pkgInfo = fmt.Sprintf("\nPackages requiring reboot:\n%s", pkgs)
			}
		}

		return fmt.Errorf("system reboot required before installation can proceed%s\n\nPlease reboot the system with: sudo reboot", pkgInfo)
	}

	return nil
}

// checkPackageManagerLocks checks if apt/dpkg processes are running
func checkPackageManagerLocks() error {
	// Check for running apt-get, apt, or dpkg processes by exact process name
	// Using -x for exact match avoids false positives like "dnsmasq"
	processNames := []string{"apt-get", "apt", "dpkg"}
	var runningPids []string

	for _, proc := range processNames {
		cmd := exec.Command("pgrep", "-x", proc)
		output, err := cmd.Output()
		if err == nil && len(output) > 0 {
			pids := strings.TrimSpace(string(output))
			runningPids = append(runningPids, fmt.Sprintf("%s(%s)", proc, pids))
		}
	}

	if len(runningPids) > 0 {
		return fmt.Errorf("package manager is currently running: %s\n\nPlease wait for package operations to complete or kill these processes", strings.Join(runningPids, ", "))
	}

	// Check for dpkg lock files using flock test
	lockFiles := []string{
		"/var/lib/dpkg/lock",
		"/var/lib/dpkg/lock-frontend",
		"/var/lib/apt/lists/lock",
	}

	for _, lockFile := range lockFiles {
		if isFileLocked(lockFile) {
			return fmt.Errorf("package manager lock file is held: %s\n\nPlease wait for package operations to complete or remove stale locks", lockFile)
		}
	}

	return nil
}

// isFileLocked checks if a file is locked by another process using flock
func isFileLocked(lockPath string) bool {
	file, err := os.Open(lockPath)
	if err != nil {
		// File doesn't exist or can't be opened - not locked
		return false
	}
	defer file.Close()

	// Try to acquire an exclusive lock without blocking
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		// Could not acquire lock - file is locked by another process
		return true
	}

	// Release the lock we just acquired
	syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false
}

// checkCPUCount ensures at least 2 CPUs are available
func checkCPUCount() error {
	cpuCount := runtime.NumCPU()
	if cpuCount < 2 {
		return fmt.Errorf("insufficient CPU cores: found %d, need at least 2\n\nResize the node to >=2 vCPU and re-run", cpuCount)
	}
	return nil
}

// checkRAM ensures at least 3.5GB of total RAM. (Available RAM + memory pressure
// are checked separately, as an advisory, by checkRamAvailableAndPressure.)
func checkRAM() error {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return fmt.Errorf("failed to read memory info: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return fmt.Errorf("failed to parse memory info")
			}
			memKB, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse memory size: %w", err)
			}
			// /proc/meminfo reports in KiB, convert to bytes then to decimal GB
			memBytes := memKB * 1024
			memGB := float64(memBytes) / 1000 / 1000 / 1000

			if memGB < 3.5 {
				return fmt.Errorf("insufficient RAM: found %.1f GB, need at least 3.5 GB\n\nResize the node to >=4 GB and re-run", memGB)
			}
			return nil
		}
	}
	return fmt.Errorf("could not determine RAM size")
}

// supportedUbuntuVersions is the set of Ubuntu LTS releases the installer
// supports end to end. Keep in sync with the Nodeward GetInstallCommands OS
// branches (per-release apt/containerd/netplan handling).
var supportedUbuntuVersions = map[string]bool{
	"22.04": true,
	"24.04": true,
	"26.04": true,
}

// supportedUbuntuList renders the supported set for user-facing messages.
const supportedUbuntuList = "22.04, 24.04, 26.04"

// checkOSVersion ensures the system runs a supported Ubuntu LTS release. It uses
// the single shared os-release parser (ID + ID_LIKE + VERSION_ID) so its verdict
// can never diverge from the OS string sent to Nodeward at registration.
func checkOSVersion() error {
	return verifyOSSupported(readOSReleaseOrEmpty())
}

// readOSReleaseOrEmpty reads /etc/os-release via the shared parser, returning a
// zero value (which verifyOSSupported reports as unsupported) on read error.
func readOSReleaseOrEmpty() commons.OSRelease {
	rel, err := commons.ReadOSRelease()
	if err != nil {
		return commons.OSRelease{}
	}
	return rel
}

// verifyOSSupported is the pure predicate behind checkOSVersion (unit-tested).
func verifyOSSupported(rel commons.OSRelease) error {
	// Operator escape hatch: an interim release (e.g. 26.10) or a close
	// derivative can be forced through with eyes open.
	if os.Getenv("RUNOS_ALLOW_UNTESTED_OS") == "1" {
		roslog.W("RUNOS_ALLOW_UNTESTED_OS=1 set; skipping OS support check", nil, "id", rel.ID, "version_id", rel.VersionID)
		return nil
	}

	if !rel.IsUbuntu() {
		detected := rel.Name
		if detected == "" {
			detected = rel.ID
		}
		if detected == "" {
			detected = "unknown"
		}
		return fmt.Errorf("unsupported OS %q: this installer supports Ubuntu LTS only (%s)\n\nProvision the node on a supported Ubuntu image and re-run, or override at your own risk with RUNOS_ALLOW_UNTESTED_OS=1", detected, supportedUbuntuList)
	}

	if !supportedUbuntuVersions[rel.VersionID] {
		return fmt.Errorf("unsupported Ubuntu version %q: supported releases are %s (LTS only)\n\nUse a supported release, or override at your own risk with RUNOS_ALLOW_UNTESTED_OS=1", rel.VersionID, supportedUbuntuList)
	}

	return nil
}

// checkDNSResolution ensures DNS is working using Go's native resolver
func checkDNSResolution() error {
	domains := []string{"github.com", "pkgs.k8s.io", "helm.cilium.io"}

	for _, domain := range domains {
		_, err := net.LookupHost(domain)
		if err != nil {
			return fmt.Errorf("DNS resolution failed for %s: %v\n\nCheck /etc/resolv.conf and network configuration", domain, err)
		}
	}

	return nil
}

// unameRelease returns the running kernel release (uname -r), or "unknown".
func unameRelease() string {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
