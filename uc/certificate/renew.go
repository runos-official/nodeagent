package certificate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/registernode"
)

// RenewalGracePeriod is how long the previously issued certificate keeps working
// after a renewal, giving the operator time to restart the agent service before
// the old certificate stops being accepted. Kept here (rather than as bare prose
// in the success message) so the value the user is told has a single source.
const RenewalGracePeriod = 5 * time.Minute

// RenewResult is the stable, machine-readable shape emitted by `runos
// certificate renew --json` on success. NotAfter is the new certificate's expiry
// (RFC3339); RestartRequired is always true because the running agent must be
// restarted to load the new certificate.
type RenewResult struct {
	Status          string `json:"status"`
	NotAfter        string `json:"notAfter"`
	RestartRequired bool   `json:"restartRequired"`
}

// certDir returns the directory the mTLS certificates live in, derived from the
// configured public-key path rather than hardcoded, so a non-default mtls.crt
// location is reported correctly.
func certDir() string {
	return filepath.Dir(config.GetPublicKeyPath())
}

// RenewCertificate renews the mTLS certificate for the node agent. It returns an
// error (already reported via roslog.Fail, or as a JSON error object in JSON
// mode) on any failure so the cobra RunE exits non-zero. When jsonOut is true,
// human progress lines are suppressed and a single RenewResult / error object is
// written to stdout.
func RenewCertificate(jsonOut bool) error {
	roslog.I("Starting certificate renewal")

	// progress prints a human-readable progress line, suppressed in JSON mode so
	// stdout carries only the final JSON object.
	progress := func(format string, args ...any) {
		if !jsonOut {
			fmt.Printf(format, args...)
		}
	}

	// fail reports a failure. In JSON mode it emits a single error object to
	// stdout and returns an already-reported error; otherwise it routes through
	// roslog.Fail (canonical block on stderr + durable log).
	fail := func(step, cause, remedy string) error {
		roslog.E(step, errors.New(cause))
		if jsonOut {
			emitJSONError(cause)
			return roslog.AlreadyReported(fmt.Errorf("%s: %s", step, cause))
		}
		return roslog.Fail(step, cause, remedy)
	}

	progress("\n╔═══════════════════════════════════════════════╗\n")
	progress("║   RunOS Certificate Renewal                   ║\n")
	progress("╚═══════════════════════════════════════════════╝\n\n")

	// Step 1: Backup existing certificates
	progress("→ Backing up existing certificates...\n")
	backupDir, err := backupCertificates()
	if err != nil {
		return fail("Renew certificate: back up existing certificates", err.Error(),
			fmt.Sprintf("ensure %s is writable and you are running as root, then re-run", certDir()))
	}
	progress("  ✓ Certificates backed up to: %s\n\n", backupDir)

	// Step 2: Connect to Nodeward and request new certificate
	progress("→ Connecting to Nodeward with current certificate...\n")
	c, _, cancel, conn, err := backend.NodewardL2Sec()
	if err != nil {
		cleanupBackup(backupDir)
		return fail("Renew certificate: connect to Nodeward", err.Error(),
			"check connectivity to Nodeward; if this node is not registered yet, run `runos register` first")
	}
	defer cancel()
	defer conn.Close()

	// Use a bounded context for the renewal RPC (mirrors autorenew.go).
	ctx, cancelCtx := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCtx()

	progress("  ✓ Connected\n\n")

	// Step 3: Call RegenerateCertificate RPC
	progress("→ Requesting new certificate from Nodeward...\n")
	roslog.I("Calling RegenerateCertificate RPC")

	request := &l2sec.RegenerateCertificateRequest{}
	response, err := c.RegenerateCertificate(ctx, request)
	if err != nil {
		cleanupBackup(backupDir)
		return fail("Renew certificate: request new certificate from Nodeward", err.Error(),
			"retry shortly; if it persists, check Nodeward availability and this node's status in the RunOS console")
	}

	progress("  ✓ New certificate received\n\n")
	roslog.I("Certificate regenerated successfully")

	// Step 4: Write new certificates to disk
	progress("→ Writing new certificates to disk...\n")
	if err := writeCertificates(response); err != nil {
		remedy := restoreOrAdvise(progress, backupDir)
		return fail("Renew certificate: write new certificates to disk", err.Error(), remedy)
	}
	progress("  ✓ New certificates written\n\n")

	// Step 5: Test new certificates by establishing connection and adding a log
	progress("→ Testing new certificates...\n")
	if err := testNewCertificates(); err != nil {
		remedy := restoreOrAdvise(progress, backupDir)
		return fail("Renew certificate: validate new certificates", err.Error(), remedy)
	}
	progress("  ✓ New certificates validated successfully\n\n")

	// Step 6: Cleanup backup files
	cleanupBackup(backupDir)

	roslog.I("Certificate renewal completed successfully")

	// Read back the new expiry for the success summary / JSON.
	notAfter, expErr := GetCertificateExpiration()
	if expErr != nil {
		// Non-fatal: renewal succeeded, we just cannot show the expiry.
		roslog.W("Renewed certificate written but expiry could not be read back", expErr)
	}

	if jsonOut {
		notAfterStr := ""
		if expErr == nil {
			notAfterStr = notAfter.Format(time.RFC3339)
		}
		return emitJSON(RenewResult{
			Status:          "renewed",
			NotAfter:        notAfterStr,
			RestartRequired: true,
		})
	}

	// Step 7: Success message (human mode)
	progress("═══════════════════════════════════════════════\n\n")
	progress("✓ Certificate Renewal Successful!\n\n")
	if expErr == nil {
		progress("New certificate valid until: %s\n", notAfter.Format(time.RFC3339))
	}
	progress("New certificates have been written to %s\n\n", certDir())
	progress("Action Required:\n")
	progress("  Please restart the node agent service to load the new certificates:\n\n")
	progress("  sudo systemctl restart runos.service\n\n")
	progress("ℹ  You have a %s grace period. The old certificate remains\n", formatGrace(RenewalGracePeriod))
	progress("   valid during this time.\n\n")

	return nil
}

// restoreOrAdvise attempts to restore the backup after a failed write/test and
// returns a remedy string describing the resulting state for the failure report.
func restoreOrAdvise(progress func(string, ...any), backupDir string) string {
	progress("→ Restoring backup certificates...\n")
	if restoreErr := restoreCertificates(backupDir); restoreErr != nil {
		progress("  ✗ Failed to restore backup: %v\n", restoreErr)
		progress("  Please manually restore certificates from: %s\n\n", backupDir)
		return fmt.Sprintf("automatic restore failed; manually restore certificates from %s, then re-run", backupDir)
	}
	progress("  ✓ Backup certificates restored\n\n")
	return "previous certificates were restored automatically; re-run once the cause is resolved"
}

// formatGrace renders a renewal grace duration as a short human string (e.g.
// "5-minute") for the success message.
func formatGrace(d time.Duration) string {
	mins := int(d.Minutes())
	if mins > 0 && d == time.Duration(mins)*time.Minute {
		return fmt.Sprintf("%d-minute", mins)
	}
	return d.String()
}

// emitJSON writes a RenewResult to stdout as indented JSON.
func emitJSON(r RenewResult) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("encoding renew result JSON: %w", err)
	}
	return nil
}

// emitJSONError writes a stable error object to stdout in JSON mode.
func emitJSONError(message string) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}{Status: "error", Error: message})
}

// backupCertificates creates a timestamped backup of existing certificates
func backupCertificates() (string, error) {
	timestamp := time.Now().Format("20060102-150405")
	backupDir := filepath.Join(certDir(), "backup", timestamp)

	// Create backup directory
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Backup files
	files := map[string]string{
		config.GetPublicKeyPath():  filepath.Join(backupDir, "mtls.crt"),
		config.GetPrivateKeyPath(): filepath.Join(backupDir, "mtls.key"),
		config.GetCACertPath():     filepath.Join(backupDir, "ca.crt"),
	}

	for src, dst := range files {
		data, err := os.ReadFile(src)
		if err != nil {
			return backupDir, fmt.Errorf("failed to read %s: %w", src, err)
		}
		if err := os.WriteFile(dst, data, 0600); err != nil {
			return backupDir, fmt.Errorf("failed to write backup %s: %w", dst, err)
		}
	}

	roslog.I("Certificates backed up", "backup_dir", backupDir)
	return backupDir, nil
}

// writeCertificates writes the new certificates to disk
func writeCertificates(response *l2sec.RegenerateCertificateResponse) error {
	// Write CA certificate
	if err := registernode.StorePublicKey(response.CaCert, config.GetCACertPath()); err != nil {
		return fmt.Errorf("failed to write CA certificate: %w", err)
	}

	// Write public certificate
	if err := registernode.StorePublicKey(response.PublicKey, config.GetPublicKeyPath()); err != nil {
		return fmt.Errorf("failed to write public certificate: %w", err)
	}

	// Write private key
	if err := registernode.StorePrivateKey(response.PrivateKey, config.GetPrivateKeyPath()); err != nil {
		return fmt.Errorf("failed to write private key: %w", err)
	}

	roslog.I("New certificates written to disk")
	return nil
}

// testNewCertificates tests the new certificates by establishing a connection and adding a log
func testNewCertificates() error {
	// Test by adding a log entry (this will establish L2SEC connection with new certificates)
	if err := backend.AddNodelog(3, "CertificateRenewal", "Certificate renewed successfully"); err != nil {
		return fmt.Errorf("failed to add nodelog with new certificate: %w", err)
	}

	roslog.I("New certificate test successful")
	return nil
}

// restoreCertificates restores certificates from backup
func restoreCertificates(backupDir string) error {
	files := map[string]string{
		filepath.Join(backupDir, "mtls.crt"): config.GetPublicKeyPath(),
		filepath.Join(backupDir, "mtls.key"): config.GetPrivateKeyPath(),
		filepath.Join(backupDir, "ca.crt"):   config.GetCACertPath(),
	}

	for src, dst := range files {
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("failed to read backup %s: %w", src, err)
		}
		if err := os.WriteFile(dst, data, 0600); err != nil {
			return fmt.Errorf("failed to restore %s: %w", dst, err)
		}
	}

	roslog.I("Certificates restored from backup", "backup_dir", backupDir)
	return nil
}

// cleanupBackup removes the backup directory
func cleanupBackup(backupDir string) {
	if err := os.RemoveAll(backupDir); err != nil {
		roslog.W("Failed to cleanup backup directory", err)
	} else {
		roslog.I("Backup directory cleaned up", "backup_dir", backupDir)
	}
}
