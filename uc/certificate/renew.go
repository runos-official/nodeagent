package certificate

import (
	"context"
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

// RenewCertificate renews the mTLS certificate for the node agent
func RenewCertificate() {
	roslog.I("Starting certificate renewal")
	fmt.Printf("\n╔═══════════════════════════════════════════════╗\n")
	fmt.Printf("║   RunOS Certificate Renewal                   ║\n")
	fmt.Printf("╚═══════════════════════════════════════════════╝\n\n")

	// Step 1: Backup existing certificates
	fmt.Printf("→ Backing up existing certificates...\n")
	backupDir, err := backupCertificates()
	if err != nil {
		fmt.Printf("  ✗ Failed to backup certificates: %v\n\n", err)
		roslog.E("Failed to backup certificates", err)
		os.Exit(1)
	}
	fmt.Printf("  ✓ Certificates backed up to: %s\n\n", backupDir)

	// Step 2: Connect to Nodeward and request new certificate
	fmt.Printf("→ Connecting to Nodeward with current certificate...\n")
	c, ctx, cancel, conn, err := backend.NodewardL2Sec()
	if err != nil {
		fmt.Printf("  ✗ Failed to connect to Nodeward: %v\n\n", err)
		roslog.E("Failed to connect to Nodeward for certificate renewal", err)
		os.Exit(1)
	}
	defer cancel()
	defer conn.Close()

	// Extend timeout for this operation
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Printf("  ✓ Connected\n\n")

	// Step 3: Call RegenerateCertificate RPC
	fmt.Printf("→ Requesting new certificate from Nodeward...\n")
	roslog.I("Calling RegenerateCertificate RPC")

	request := &l2sec.RegenerateCertificateRequest{}
	response, err := c.RegenerateCertificate(ctx, request)
	if err != nil {
		fmt.Printf("  ✗ Failed to regenerate certificate: %v\n\n", err)
		roslog.E("Failed to regenerate certificate", err)
		cleanupBackup(backupDir)
		os.Exit(1)
	}

	fmt.Printf("  ✓ New certificate received\n\n")
	roslog.I("Certificate regenerated successfully")

	// Step 4: Write new certificates to disk
	fmt.Printf("→ Writing new certificates to disk...\n")
	if err := writeCertificates(response); err != nil {
		fmt.Printf("  ✗ Failed to write certificates: %v\n\n", err)
		roslog.E("Failed to write certificates", err)
		fmt.Printf("→ Restoring backup certificates...\n")
		if restoreErr := restoreCertificates(backupDir); restoreErr != nil {
			fmt.Printf("  ✗ Failed to restore backup: %v\n", restoreErr)
			fmt.Printf("  Please manually restore certificates from: %s\n\n", backupDir)
		} else {
			fmt.Printf("  ✓ Backup certificates restored\n\n")
		}
		os.Exit(1)
	}
	fmt.Printf("  ✓ New certificates written\n\n")

	// Step 5: Test new certificates by establishing connection and adding log
	fmt.Printf("→ Testing new certificates...\n")
	if err := testNewCertificates(); err != nil {
		fmt.Printf("  ✗ Certificate test failed: %v\n\n", err)
		roslog.E("New certificate test failed", err)
		fmt.Printf("→ Restoring backup certificates...\n")
		if restoreErr := restoreCertificates(backupDir); restoreErr != nil {
			fmt.Printf("  ✗ Failed to restore backup: %v\n", restoreErr)
			fmt.Printf("  Please manually restore certificates from: %s\n\n", backupDir)
		} else {
			fmt.Printf("  ✓ Backup certificates restored\n\n")
		}
		os.Exit(1)
	}
	fmt.Printf("  ✓ New certificates validated successfully\n\n")

	// Step 6: Cleanup backup files
	cleanupBackup(backupDir)

	// Step 7: Success message
	fmt.Printf("═══════════════════════════════════════════════\n\n")
	fmt.Printf("✓ Certificate Renewal Successful!\n\n")
	fmt.Printf("New certificates have been written to /etc/runos/\n\n")
	fmt.Printf("Action Required:\n")
	fmt.Printf("  Please restart the node agent service to load the new certificates:\n\n")
	fmt.Printf("  sudo systemctl restart runos.service\n\n")
	fmt.Printf("ℹ  You have a 5-minute grace period. The old certificate remains\n")
	fmt.Printf("   valid during this time.\n\n")

	roslog.I("Certificate renewal completed successfully")
}

// backupCertificates creates a timestamped backup of existing certificates
func backupCertificates() (string, error) {
	timestamp := time.Now().Format("20060102-150405")
	backupDir := filepath.Join("/etc/runos/backup", timestamp)

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
