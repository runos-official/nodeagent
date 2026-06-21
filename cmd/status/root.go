package status

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/k8s"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/version"

	"github.com/spf13/cobra"
)

// logFilePath is the single source of truth for the durable log location that
// this command reports on. It mirrors roslog's own sink; kept here as a local
// const so status does not reach into roslog internals.
const logFilePath = "/var/log/runos.log"

// notSet / notRegistered are the placeholders rendered for empty config values
// so a half-configured node is obvious instead of showing a blank field.
const (
	notSet        = "(not set)"
	notRegistered = "(not registered)"
)

// ErrWg0NotFound is returned by getWg0IP when the wg0 interface is not present,
// so callers can distinguish "VPN not configured" from other failures via
// errors.Is rather than matching on message text.
var ErrWg0NotFound = errors.New("wg0 interface not found")

var (
	jsonOutput bool
	probeWrite bool
)

var RootCmd = &cobra.Command{
	Use:   "status",
	Short: "Display the current status of the Node Agent",
	Long: `Display the current status of the Node Agent.

Reports the agent version, Nodeward connectivity, Kubernetes node role and
readiness, node configuration, mTLS certificate expiry, and the local log file.

The command is read-only by default: it never mutates remote state. Use
--probe-write to additionally send a single test log line to Nodeward when you
want to verify the write path end to end.

Exit status is non-zero when a hard failure is detected (cannot reach Nodeward,
or the mTLS certificate is missing/unreadable/expired) so the command can be
used in health checks and shell "&&" chains.`,
	Example: `  runos status
  runos status --json
  runos status --probe-write`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return displayStatus()
	},
}

func init() {
	RootCmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit status as a single JSON object to stdout")
	RootCmd.Flags().BoolVar(&probeWrite, "probe-write", false, "Also send one test log line to Nodeward to verify the write path (mutates remote state)")
}

// k8sStatus is the Kubernetes section of the JSON output.
type k8sStatus struct {
	Installed bool   `json:"installed"`
	NodeType  string `json:"nodeType"`
	Status    string `json:"status"`
}

// configStatus is the node configuration section of the JSON output.
type configStatus struct {
	AID          string `json:"aid"`
	NID          string `json:"nid"`
	NodeIP       string `json:"nodeIp"`
	ExternalIP   string `json:"externalIp"`
	VPNIP        string `json:"vpnIp"`
	NodewardHost string `json:"nodewardHost"`
}

// certStatus is the mTLS certificate section of the JSON output.
type certStatus struct {
	Path          string `json:"path"`
	Expires       string `json:"expires"`
	DaysRemaining int    `json:"daysRemaining"`
	Expired       bool   `json:"expired"`
	Error         string `json:"error,omitempty"`
}

// logStatus is the log file section of the JSON output.
type logStatus struct {
	Path  string `json:"path"`
	Size  string `json:"size"`
	Error string `json:"error,omitempty"`
}

// statusReport is the stable struct emitted by --json.
type statusReport struct {
	Version      string       `json:"version"`
	Binary       string       `json:"binary"`
	Connected    bool         `json:"connected"`
	NodewardHost string       `json:"nodewardHost"`
	K8s          k8sStatus    `json:"k8s"`
	Config       configStatus `json:"config"`
	Cert         certStatus   `json:"cert"`
	Log          logStatus    `json:"log"`
	Degraded     bool         `json:"degraded"`
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

// testConnection attempts to connect to Nodeward L2Sec. It returns
// (connected, err); on any failure connected is false and err carries the
// reason. The deferred recover converts a panic in the dialing path into a
// proper error return rather than crashing the command or silently reporting
// "not connected" with no cause.
func testConnection() (connected bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			connected = false
			err = fmt.Errorf("connection panicked: %v", r)
		}
	}()

	_, _, cancel, conn, dialErr := backend.NodewardL2Sec()
	if dialErr != nil || conn == nil {
		if dialErr == nil {
			dialErr = errors.New("no connection established")
		}
		return false, fmt.Errorf("connection failed: %w", dialErr)
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
	info, err := os.Stat(logFilePath)
	if err != nil {
		return logFilePath, "", err
	}
	return logFilePath, formatBytes(info.Size()), nil
}

// getWg0IP returns the first IPv4 address assigned to the wg0 interface using
// the net package as the single source of truth (no shelling out). It returns
// ErrWg0NotFound when the interface does not exist, so callers can distinguish
// "VPN not configured" from "configured but no address".
func getWg0IP() (string, error) {
	iface, err := net.InterfaceByName("wg0")
	if err != nil {
		// InterfaceByName fails when the interface is absent.
		return "", ErrWg0NotFound
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("failed to read wg0 addresses: %w", err)
	}

	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip4 := ip.To4(); ip4 != nil {
			return ip4.String(), nil
		}
	}

	return "", fmt.Errorf("no IPv4 address assigned")
}

// orPlaceholder returns the value, or the supplied placeholder when empty.
func orPlaceholder(value, placeholder string) string {
	if value == "" {
		return placeholder
	}
	return value
}

// gatherReport collects every status field once, independent of output format,
// and reports whether a hard failure (degraded) was detected. Hard failures:
// cannot reach Nodeward, or the mTLS certificate is missing/unreadable/expired.
func gatherReport() statusReport {
	r := statusReport{
		Version:      version.Version,
		NodewardHost: config.GetNodewardHost(),
	}

	if exe, err := os.Executable(); err == nil {
		r.Binary = exe
	} else {
		r.Binary = os.Args[0]
	}

	// Connection.
	connected, connErr := testConnection()
	r.Connected = connected
	if !connected {
		r.Degraded = true
	}
	_ = connErr // surfaced by the human renderer; JSON exposes Connected/Degraded.

	// Kubernetes.
	r.K8s.Installed = k8s.IsInstalled()
	if r.K8s.Installed {
		isCp := k8s.IsCP()
		isWorker := k8s.IsWorker()
		switch {
		case isCp && isWorker:
			r.K8s.NodeType = "control-plane (can run workloads)"
		case isCp:
			r.K8s.NodeType = "control-plane (dedicated)"
		case isWorker:
			r.K8s.NodeType = "worker"
		default:
			r.K8s.NodeType = "unknown"
		}
		r.K8s.Status = k8s.GetStatus()
	}

	// Config.
	r.Config.AID = config.GetAID()
	r.Config.NID = config.GetNID()
	r.Config.NodeIP = config.GetNodeIP()
	r.Config.NodewardHost = config.GetNodewardHost()
	if externalIP, err := commons.GetExternalIPAddress(); err == nil {
		r.Config.ExternalIP = externalIP
	}
	if wg0IP, err := getWg0IP(); err == nil {
		r.Config.VPNIP = wg0IP
	}

	// Certificate.
	r.Cert.Path = config.GetPublicKeyPath()
	expiration, certErr := getCertificateExpiration()
	if certErr != nil {
		r.Cert.Error = certErr.Error()
		r.Degraded = true
	} else {
		days := int(time.Until(expiration).Hours() / 24)
		r.Cert.Expires = expiration.Format("2006-01-02 15:04:05 MST")
		r.Cert.DaysRemaining = days
		if days < 0 {
			r.Cert.Expired = true
			r.Degraded = true
		}
	}

	// Log file.
	logPath, logSize, logErr := getLogFileInfo()
	r.Log.Path = logPath
	if logErr != nil {
		r.Log.Error = logErr.Error()
	} else {
		r.Log.Size = logSize
	}

	return r
}

// displayStatus shows the node agent status information. It returns a non-nil
// error when a hard failure is detected so the command exits non-zero (usable
// in health checks / "&&" chains). The error is already-reported (its detail is
// printed via roslog.Fail to stderr) so main.go does not double-print.
func displayStatus() error {
	r := gatherReport()

	// Optional explicit write probe (off by default; status is read-only).
	if probeWrite {
		if err := backend.AddNodelog(1, "STATUS_CHECK", "Status command executed (--probe-write)"); err != nil {
			// Diagnostic detail to stderr; not a hard failure on its own.
			fmt.Fprintf(os.Stderr, "warning: write probe failed: %v\n", err)
			r.Degraded = true
		}
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			return roslog.Fail(
				"render status as JSON",
				fmt.Sprintf("could not encode the status report: %v", err),
				"retry without --json, or report this if it persists",
			)
		}
		if r.Degraded {
			return roslog.AlreadyReported(errors.New("node agent status is degraded"))
		}
		return nil
	}

	renderHuman(r)

	if r.Degraded {
		// One canonical failure block to stderr summarizing the hard failure,
		// with a remedy. Detail already shown inline above on stdout/stderr.
		return failForDegraded(r)
	}
	return nil
}

// failForDegraded emits a single canonical Fail block describing the most
// actionable hard failure and returns the already-reported error.
func failForDegraded(r statusReport) error {
	switch {
	case r.Cert.Error != "":
		return roslog.Fail(
			"read mTLS certificate",
			fmt.Sprintf("%s at %s", r.Cert.Error, r.Cert.Path),
			"run `runos register` to (re)issue node certificates",
		)
	case r.Cert.Expired:
		return roslog.Fail(
			"validate mTLS certificate",
			fmt.Sprintf("certificate at %s expired %d days ago", r.Cert.Path, -r.Cert.DaysRemaining),
			"run `runos certificate renew` to obtain a fresh certificate",
		)
	case !r.Connected:
		return roslog.Fail(
			"connect to Nodeward",
			fmt.Sprintf("could not establish an mTLS stream to %s", r.NodewardHost),
			"check network reachability and that `runos register` has been run on this node",
		)
	default:
		return roslog.AlreadyReported(errors.New("node agent status is degraded"))
	}
}

// renderHuman prints the formatted, aligned human-readable status block to
// stdout (primary data) and routes failure/diagnostic detail to stderr.
func renderHuman(r statusReport) {
	roslog.Printf("\n╔═══════════════════════════════════════════════╗\n")
	roslog.Printf("║   RunOS Node Agent Status                     ║\n")
	roslog.Printf("╚═══════════════════════════════════════════════╝\n\n")

	// Version Information.
	roslog.Printf("Agent Version:\n")
	roslog.Printf("  Version: %s\n", r.Version)
	roslog.Printf("  Binary:  %s\n\n", r.Binary)

	// Connection Status.
	roslog.Printf("Connection Status:\n")
	if r.Connected {
		roslog.Printf("  ✓ Connected to Nodeward: %s\n", r.NodewardHost)
	} else {
		roslog.Printf("  ✗ Not connected to Nodeward: %s\n", r.NodewardHost)
		fmt.Fprintf(os.Stderr, "  could not establish an mTLS stream to %s; run `runos register` if this node is not registered\n", r.NodewardHost)
	}
	roslog.Printf("\n")

	// Kubernetes Status.
	roslog.Printf("Kubernetes Status:\n")
	if r.K8s.Installed {
		roslog.Printf("  ✓ Kubernetes is installed\n")
		roslog.Printf("  → Node Type: %s\n", r.K8s.NodeType)
		switch r.K8s.Status {
		case "ready":
			roslog.Printf("  ✓ Node Status: Ready\n")
		case "not_ready":
			roslog.Printf("  ✗ Node Status: Not Ready\n")
		case "cordoned":
			roslog.Printf("  ℹ Node Status: Cordoned\n")
		default:
			roslog.Printf("  ℹ Node Status: %s\n", r.K8s.Status)
		}
	} else {
		roslog.Printf("  ✗ Kubernetes is not installed\n")
	}
	roslog.Printf("\n")

	// Node Configuration.
	roslog.Printf("Node Configuration:\n")
	roslog.Printf("  Account ID (AID):  %s\n", orPlaceholder(r.Config.AID, notRegistered))
	roslog.Printf("  Node ID (NID):     %s\n", orPlaceholder(r.Config.NID, notRegistered))
	roslog.Printf("  Node IP:           %s\n", orPlaceholder(r.Config.NodeIP, notSet))
	roslog.Printf("  External IP:       %s\n", orPlaceholder(r.Config.ExternalIP, notSet))
	roslog.Printf("  VPN IP (wg0):      %s\n", orPlaceholder(r.Config.VPNIP, "(not configured)"))
	roslog.Printf("  Nodeward Server:   %s\n", orPlaceholder(r.Config.NodewardHost, notSet))
	roslog.Printf("\n")

	// Certificate Status.
	roslog.Printf("mTLS Certificate:\n")
	roslog.Printf("  Certificate Path:  %s\n", orPlaceholder(r.Cert.Path, notSet))
	if r.Cert.Error != "" {
		roslog.Printf("  ✗ Certificate not accessible\n")
		fmt.Fprintf(os.Stderr, "  %s\n", r.Cert.Error)
		roslog.E("Failed to read certificate", errors.New(r.Cert.Error))
	} else {
		roslog.Printf("  Expires:           %s\n", r.Cert.Expires)
		switch {
		case r.Cert.Expired:
			roslog.Printf("  ✗ Certificate EXPIRED %d days ago\n", -r.Cert.DaysRemaining)
		case r.Cert.DaysRemaining < 7:
			roslog.Printf("  ℹ Certificate expires in %d days (renewal recommended)\n", r.Cert.DaysRemaining)
		case r.Cert.DaysRemaining < 30:
			roslog.Printf("  ℹ Certificate expires in %d days\n", r.Cert.DaysRemaining)
		default:
			roslog.Printf("  ✓ Certificate expires in %d days\n", r.Cert.DaysRemaining)
		}
	}
	roslog.Printf("\n")

	// Log File Information.
	roslog.Printf("Log File:\n")
	roslog.Printf("  Log File:          %s\n", r.Log.Path)
	if r.Log.Error != "" {
		roslog.Printf("  ✗ Not accessible\n")
		fmt.Fprintf(os.Stderr, "  %s\n", r.Log.Error)
	} else {
		roslog.Printf("  ✓ Size:            %s\n", r.Log.Size)
	}

	roslog.Printf("\n═══════════════════════════════════════════════\n\n")
}
