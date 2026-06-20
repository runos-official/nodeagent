package status

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/k8s"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/version"

	"github.com/spf13/cobra"
)

// ErrWg0NotFound is returned by getWg0IP when the wg0 interface is not present,
// so callers can distinguish "VPN not configured" from other failures via
// errors.Is rather than matching on message text.
var ErrWg0NotFound = errors.New("wg0 interface not found")

var RootCmd = &cobra.Command{
	Use:   "status",
	Short: "Display the current status of the Node Agent",
	Run: func(cmd *cobra.Command, args []string) {
		displayStatus()
	},
}

// getCertificateExpiration reads the mTLS certificate and returns expiration info
func getCertificateExpiration() (time.Time, error) {
	certPath := config.GetPublicKeyPath()
	data, err := os.ReadFile(certPath)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to read certificate: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return time.Time{}, fmt.Errorf("failed to decode PEM block")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse certificate: %w", err)
	}

	return cert.NotAfter, nil
}

// testConnection attempts to connect to Nodeward L2Sec
func testConnection() (bool, error) {
	defer func() {
		// Recover from panic if connection fails
		if r := recover(); r != nil {
			// Connection failed
		}
	}()

	_, _, cancel, conn := backend.NodewardL2Sec()
	if conn == nil {
		return false, fmt.Errorf("connection failed")
	}
	defer cancel()
	defer conn.Close()
	return true, nil
}

// formatBytes converts bytes to human-readable format
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// getLogFileInfo returns log file path and size
func getLogFileInfo() (string, string, error) {
	logPath := "/var/log/runos.log"
	info, err := os.Stat(logPath)
	if err != nil {
		return logPath, "", err
	}
	return logPath, formatBytes(info.Size()), nil
}

// getWg0IP gets the IP address of the wg0 interface
func getWg0IP() (string, error) {
	// Try to get wg0 IP using 'ip addr show wg0'
	cmd := fmt.Sprintf("ip addr show wg0 | grep 'inet ' | awk '{print $2}' | cut -d'/' -f1")
	output, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return "", fmt.Errorf("network interfaces not accessible")
	}

	// Check if wg0 exists
	if !strings.Contains(string(output), "wg0:") {
		return "", ErrWg0NotFound
	}

	// Execute command to get IP
	result, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		return "", fmt.Errorf("failed to get wg0 IP")
	}

	ip := strings.TrimSpace(string(result))
	if ip == "" {
		return "", fmt.Errorf("no IP address assigned")
	}

	return ip, nil
}

// displayStatus shows the node agent status information
func displayStatus() {
	fmt.Printf("\n╔═══════════════════════════════════════════════╗\n")
	fmt.Printf("║   RunOS Node Agent Status                     ║\n")
	fmt.Printf("╚═══════════════════════════════════════════════╝\n\n")

	// Version Information
	fmt.Printf("Agent Version:\n")
	fmt.Printf("  Version: %s\n", version.Version)
	fmt.Printf("  Binary:  /usr/local/bin/runos\n\n")

	// Test Log Message
	fmt.Printf("Test Log Message:\n")
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("  ✗ Failed to send log to Nodeward: %v\n", r)
			}
		}()
		err := backend.AddNodelog(1, "STATUS_CHECK", "Status command executed")
		if err != nil {
			fmt.Printf("  ✗ Failed to send log to Nodeward: %v\n", err)
		} else {
			fmt.Printf("  ✓ Successfully sent test log to Nodeward\n")
		}
	}()
	fmt.Printf("\n")

	// Stream Connection Status
	fmt.Printf("Connection Status:\n")
	connected, connErr := testConnection()
	if connected {
		fmt.Printf("  ✓ Connected to Nodeward: %s\n", config.GetNodewardHost())
	} else {
		fmt.Printf("  ✗ Failed to connect to Nodeward: %s", config.GetNodewardHost())
		if connErr != nil {
			fmt.Printf(" (%v)", connErr)
		}
		fmt.Printf("\n")
	}
	fmt.Printf("\n")

	// Kubernetes Status
	fmt.Printf("Kubernetes Status:\n")
	if k8s.IsInstalled() {
		fmt.Printf("  ✓ Kubernetes is installed\n")

		isCp := k8s.IsCP()
		isWorker := k8s.IsWorker()

		if isCp && isWorker {
			fmt.Printf("  → Node Type: Control Plane (can run workloads)\n")
		} else if isCp {
			fmt.Printf("  → Node Type: Control Plane (dedicated)\n")
		} else if isWorker {
			fmt.Printf("  → Node Type: Worker\n")
		} else {
			fmt.Printf("  → Node Type: Unknown\n")
		}

		status := k8s.GetStatus()
		switch status {
		case "ready":
			fmt.Printf("  ✓ Node Status: Ready\n")
		case "not_ready":
			fmt.Printf("  ✗ Node Status: Not Ready\n")
		case "cordoned":
			fmt.Printf("  ℹ  Node Status: Cordoned\n")
		default:
			fmt.Printf("  ℹ  Node Status: %s\n", status)
		}
	} else {
		fmt.Printf("  ✗ Kubernetes is not installed\n")
	}
	fmt.Printf("\n")

	// Node Configuration
	fmt.Printf("Node Configuration:\n")
	fmt.Printf("  Account ID (AID):  %s\n", config.GetAID())
	fmt.Printf("  Node ID (NID):     %s\n", config.GetNID())
	fmt.Printf("  Node IP:           %s\n", config.GetNodeIP())

	// External IP
	externalIP, err := commons.GetExternalIPAddress()
	if err != nil {
		fmt.Printf("  External IP:       Not available\n")
	} else {
		fmt.Printf("  External IP:       %s\n", externalIP)
	}

	// VPN IP (wg0)
	wg0IP, wg0Err := getWg0IP()
	if wg0Err != nil {
		if errors.Is(wg0Err, ErrWg0NotFound) {
			fmt.Printf("  VPN IP (wg0):      Not configured\n")
		} else {
			fmt.Printf("  VPN IP (wg0):      Not available\n")
		}
	} else {
		fmt.Printf("  VPN IP (wg0):      %s\n", wg0IP)
	}

	fmt.Printf("  Nodeward Server:   %s\n", config.GetNodewardHost())
	fmt.Printf("\n")

	// Certificate Status
	fmt.Printf("mTLS Certificate:\n")
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("  ✗ Certificate not accessible: %v\n", r)
			}
		}()

		certPath := config.GetPublicKeyPath()
		expiration, err := getCertificateExpiration()
		if err != nil {
			fmt.Printf("  ✗ Failed to read certificate: %v\n", err)
			roslog.E("Failed to read certificate", err)
		} else {
			now := time.Now()
			daysRemaining := int(expiration.Sub(now).Hours() / 24)

			fmt.Printf("  Certificate Path:  %s\n", certPath)
			fmt.Printf("  Expires:           %s\n", expiration.Format("2006-01-02 15:04:05 MST"))

			if daysRemaining < 0 {
				fmt.Printf("  ✗ Certificate EXPIRED %d days ago\n", -daysRemaining)
			} else if daysRemaining < 7 {
				fmt.Printf("  ℹ  Certificate expires in %d days (renewal recommended)\n", daysRemaining)
			} else if daysRemaining < 30 {
				fmt.Printf("  ℹ  Certificate expires in %d days\n", daysRemaining)
			} else {
				fmt.Printf("  ✓ Certificate expires in %d days\n", daysRemaining)
			}
		}
	}()
	fmt.Printf("\n")

	// Log File Information
	fmt.Printf("Log File:\n")
	logPath, logSize, err := getLogFileInfo()
	if err != nil {
		fmt.Printf("  Log File:          %s\n", logPath)
		fmt.Printf("  ✗ Not accessible: %v\n", err)
	} else {
		fmt.Printf("  Log File:          %s\n", logPath)
		fmt.Printf("  ✓ Size:            %s\n", logSize)
	}

	fmt.Printf("\n═══════════════════════════════════════════════\n\n")
}
