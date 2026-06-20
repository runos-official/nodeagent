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

	"github.com/spf13/cobra"
)

var RootCmd = &cobra.Command{
	Use:   "preflight",
	Short: "Check if the system is ready for installation",
	Long:  `Performs pre-flight checks to ensure the system is ready for node registration and installation`,
	Run: func(cmd *cobra.Command, args []string) {
		if err := runPreflightChecks(); err != nil {
			fmt.Fprintf(os.Stderr, "Preflight check failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("System is ready for installation")
	},
}

func runPreflightChecks() error {
	// Check if preflight checks should be skipped (dev mode)
	if os.Getenv("RUNOS_DEV_SKIP_PREFLIGHT") == "1" {
		fmt.Println("RUNOS_DEV_SKIP_PREFLIGHT=1 is set, skipping preflight checks")
		return nil
	}

	if err := checkSystemRequirements(); err != nil {
		return err
	}

	if err := checkRebootRequired(); err != nil {
		return err
	}

	if err := checkPackageManagerLocks(); err != nil {
		return err
	}

	// Check DNS and network connectivity BEFORE apt-get update
	// so we get fast, clear errors if network is broken
	if err := checkDNSResolution(); err != nil {
		return err
	}

	if err := checkNetworkConnectivity(); err != nil {
		return err
	}

	if err := checkBrokenAptSources(); err != nil {
		return err
	}

	if err := checkKernelModules(); err != nil {
		return err
	}

	if err := checkConflictingServices(); err != nil {
		return err
	}

	return nil
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

// checkOSVersion ensures the system is running Ubuntu 24.04
func checkOSVersion() error {
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return fmt.Errorf("failed to read OS release info: %w", err)
	}
	defer file.Close()

	var osName, osVersion string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "NAME=") {
			osName = strings.Trim(strings.TrimPrefix(line, "NAME="), "\"")
		}
		if strings.HasPrefix(line, "VERSION_ID=") {
			osVersion = strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), "\"")
		}
	}

	if !strings.Contains(strings.ToLower(osName), "ubuntu") {
		return fmt.Errorf("unsupported OS: found %s, need Ubuntu 22.04 or 24.04", osName)
	}

	if osVersion != "22.04" && osVersion != "24.04" {
		return fmt.Errorf("unsupported Ubuntu version: found %s, need 22.04 or 24.04", osVersion)
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

	// Check wireguard separately as it's critical
	// On Ubuntu 24.04 (kernel 6.8+), WireGuard is built into the kernel
	cmd := exec.Command("modprobe", "--dry-run", "wireguard")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("WireGuard kernel module not available\n\nOn Ubuntu 24.04, WireGuard is built into the kernel.\nTry running: sudo modprobe wireguard\nIf that fails, ensure you have the correct kernel: uname -r")
	}

	return nil
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
