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

var RootCmd = &cobra.Command{
	Use:   "preflight",
	Short: "Check if the system is ready for installation",
	Long:  `Performs pre-flight checks to ensure the system is ready for node registration and installation`,
	Run: func(cmd *cobra.Command, args []string) {
		if err := runPreflightChecks(); err != nil {
			// Durable record for `runos logs`, then the terminal message.
			roslog.E("preflight check failed", err)
			fmt.Fprintf(os.Stderr, "Preflight check failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("System is ready for installation")
	},
}

func init() {
	RootCmd.PersistentFlags().StringVarP(&server, "server", "s", "",
		"Nodeward host this node will register against (probed for reachability)")
}

func runPreflightChecks() error {
	// Check if preflight checks should be skipped (dev mode)
	if os.Getenv("RUNOS_DEV_SKIP_PREFLIGHT") == "1" {
		fmt.Println("RUNOS_DEV_SKIP_PREFLIGHT=1 is set, skipping preflight checks")
		return nil
	}

	// Run the cheapest, most fundamental check first: everything downstream
	// (writing /etc/runos, modprobe, apt, kubeadm) needs root, and a non-root
	// run otherwise fails deep inside with a cryptic permission error.
	if err := checkRoot(); err != nil {
		return err
	}

	// Local, cheap checks before any network I/O so the fast failures fire first.
	if err := checkArch(); err != nil {
		return err
	}

	if err := checkSystemRequirements(); err != nil {
		return err
	}

	if err := checkSwap(); err != nil {
		return err
	}

	if err := checkPortsFree(); err != nil {
		return err
	}

	if err := checkRebootRequired(); err != nil {
		return err
	}

	if err := checkPackageManagerLocks(); err != nil {
		return err
	}

	if err := checkKernelModules(); err != nil {
		return err
	}

	if err := checkConflictingServices(); err != nil {
		return err
	}

	// Clock skew breaks TLS handshakes and apt Release-file validation, so check
	// it before the network checks (which would otherwise fail cryptically).
	if err := checkClockSkew(); err != nil {
		return err
	}

	// Network checks last. DNS before HTTPS so a broken resolver fails fast.
	if err := checkDNSResolution(); err != nil {
		return err
	}

	if err := checkNetworkConnectivity(); err != nil {
		return err
	}

	if err := checkNodewardReachable(); err != nil {
		return err
	}

	if err := checkBrokenAptSources(); err != nil {
		return err
	}

	return nil
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
	if strings.TrimSpace(string(out)) != "yes" {
		return fmt.Errorf("system clock is not NTP-synchronized; TLS handshakes to Nodeward and package mirrors may fail with 'certificate not yet valid/expired'\n\nFix with:\n  sudo timedatectl set-ntp true\nThen wait ~10s, verify with 'timedatectl', and re-run")
	}
	return nil
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

// checkSystemRequirements checks if the system meets minimum requirements
func checkSystemRequirements() error {
	// Check CPU count
	if err := checkCPUCount(); err != nil {
		return err
	}

	// Check RAM
	if err := checkRAM(); err != nil {
		return err
	}

	// Check disk space
	if err := checkDiskSpace(); err != nil {
		return err
	}

	// Check OS version
	if err := checkOSVersion(); err != nil {
		return err
	}

	return nil
}

// checkCPUCount ensures at least 2 CPUs are available
func checkCPUCount() error {
	cpuCount := runtime.NumCPU()
	if cpuCount < 2 {
		return fmt.Errorf("insufficient CPU cores: found %d, need at least 2", cpuCount)
	}
	return nil
}

// checkRAM ensures at least 3.5GB of RAM is available
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
				return fmt.Errorf("insufficient RAM: found %.1f GB, need at least 3.5 GB", memGB)
			}
			return nil
		}
	}
	return fmt.Errorf("could not determine RAM size")
}

// checkDiskSpace ensures at least 25GB of disk space is available on root partition
func checkDiskSpace() error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return fmt.Errorf("failed to get disk space info: %w", err)
	}

	// Available space in bytes
	availableBytes := stat.Bavail * uint64(stat.Bsize)
	// Use decimal GB (base 10) to match df -H output
	availableGB := availableBytes / 1000 / 1000 / 1000

	if availableGB < 25 {
		return fmt.Errorf("insufficient disk space: found %d GB available, need at least 25 GB", availableGB)
	}
	return nil
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

// checkNetworkConnectivity ensures the system can reach required external endpoints
func checkNetworkConnectivity() error {
	endpoints := []struct {
		name string
		url  string
	}{
		{"Kubernetes packages", "https://pkgs.k8s.io"},
		{"Helm Cilium repo", "https://helm.cilium.io"},
		{"GitHub", "https://github.com"},
		{"Kubernetes registry", "https://registry.k8s.io"},
		{"Docker Hub", "https://registry-1.docker.io"},
	}

	for _, ep := range endpoints {
		cmd := exec.Command("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
			"--connect-timeout", "10", "--max-time", "15", ep.url)
		output, err := cmd.Output()

		if err != nil {
			return fmt.Errorf("cannot reach %s (%s): network timeout or connection refused\n\nCheck firewall rules, DNS settings, or proxy configuration", ep.name, ep.url)
		}

		// Accept 2xx, 3xx as success. Also accept 401/403/404 since they prove
		// the server is reachable (registries often return these at root URL)
		code := string(output)
		codeInt, _ := strconv.Atoi(code)
		if codeInt == 0 || (codeInt >= 500) {
			return fmt.Errorf("cannot reach %s (%s): received HTTP %s", ep.name, ep.url, code)
		}
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

// checkKernelModules ensures required kernel modules can be loaded
func checkKernelModules() error {
	modules := []string{"br_netfilter", "overlay", "nf_conntrack"}

	for _, mod := range modules {
		cmd := exec.Command("modprobe", "--dry-run", mod)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("kernel module '%s' cannot be loaded\n\nThis may require a different kernel or missing kernel headers", mod)
		}
	}

	// Check wireguard separately as it's critical. WireGuard is in-kernel on
	// Linux 5.6+, so all supported Ubuntu LTS releases have it; reference the
	// detected release + kernel rather than a hardcoded version.
	cmd := exec.Command("modprobe", "--dry-run", "wireguard")
	if err := cmd.Run(); err != nil {
		rel := readOSReleaseOrEmpty()
		release := rel.VersionID
		if release == "" {
			release = "this system"
		}
		return fmt.Errorf("WireGuard kernel module not available (running Ubuntu %s on kernel %s)\n\nOn supported Ubuntu LTS releases WireGuard is in-kernel. Try: sudo modprobe wireguard\nIf that fails, install linux-modules-extra-$(uname -r) or boot the generic/HWE kernel, then retry", release, unameRelease())
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

// checkBrokenAptSources checks for broken apt sources that would cause apt-get update to fail
func checkBrokenAptSources() error {
	cmd := exec.Command("apt-get", "update", "-qq")
	output, err := cmd.CombinedOutput()

	if err != nil {
		outputStr := string(output)
		// Look for specific error patterns
		if strings.Contains(outputStr, "does not have a Release file") ||
			strings.Contains(outputStr, "404  Not Found") {
			return fmt.Errorf("broken APT sources detected:\n%s\n\nRemove or fix the broken repository in /etc/apt/sources.list.d/", outputStr)
		}
		return fmt.Errorf("apt-get update failed: %s", outputStr)
	}

	return nil
}

// checkConflictingServices checks for services that might interfere with installation
func checkConflictingServices() error {
	conflicts := []struct {
		service string
		warning string
	}{
		{"k3s", "K3s is installed and may conflict with kubeadm"},
		{"k0s", "K0s is installed and may conflict with kubeadm"},
		{"microk8s", "MicroK8s is installed and may conflict with kubeadm"},
		{"docker", "Docker is installed (containerd will be used instead - this is just a warning)"},
	}

	var warnings []string
	for _, c := range conflicts {
		cmd := exec.Command("systemctl", "is-active", "--quiet", c.service)
		if cmd.Run() == nil {
			warnings = append(warnings, c.warning)
		}
	}

	// Check for existing kubernetes installation
	if _, err := os.Stat("/etc/kubernetes/admin.conf"); err == nil {
		return fmt.Errorf("existing Kubernetes installation detected at /etc/kubernetes/\n\nRun 'kubeadm reset -f' to clean up first")
	}

	// Warnings are non-fatal, just log them
	if len(warnings) > 0 {
		fmt.Printf("Warnings:\n- %s\n", strings.Join(warnings, "\n- "))
	}

	return nil
}
