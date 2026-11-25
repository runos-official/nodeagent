package certificate

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certpolicy"
	"github.com/runos-official/nodeagent/uc/registernode"
)

// GetCertificateExpiration reads the mTLS certificate and returns its expiration time
func GetCertificateExpiration() (time.Time, error) {
	_, notAfter, err := GetCertificateValidity()
	return notAfter, err
}

// GetCertificateValidity reads the mTLS certificate and returns its
// NotBefore and NotAfter.
func GetCertificateValidity() (notBefore, notAfter time.Time, err error) {
	certPath := config.GetPublicKeyPath()
	data, err := os.ReadFile(certPath)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("failed to read certificate: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return time.Time{}, time.Time{}, fmt.Errorf("failed to decode PEM block")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("failed to parse certificate: %w", err)
	}

	return cert.NotBefore, cert.NotAfter, nil
}

// RenewalTiming gives the renewal timing of the held certificate at now.
// The due point follows the certificate's own life (certpolicy.Evaluate).
func RenewalTiming(now time.Time) (certpolicy.Timing, error) {
	notBefore, notAfter, err := GetCertificateValidity()
	if err != nil {
		return certpolicy.Timing{}, err
	}
	return renewalTiming(notBefore, notAfter, config.GetNID(), now), nil
}

// renewalTiming is the pure part of RenewalTiming.
func renewalTiming(notBefore, notAfter time.Time, nodeID string, now time.Time) certpolicy.Timing {
	return certpolicy.Evaluate(notBefore, notAfter, nodeID, now)
}

// CheckAndAutoRenew checks if the certificate is expiring within the threshold
// and automatically renews it if needed. Returns true if renewal was performed.
func CheckAndAutoRenew() (bool, error) {
	now := time.Now()
	timing, err := RenewalTiming(now)
	if err != nil {
		return false, fmt.Errorf("failed to check certificate expiration: %w", err)
	}
	expiration := timing.NotAfter
	daysRemaining := int(expiration.Sub(now).Hours() / 24)

	roslog.I("Certificate expiry check", "expires", expiration.Format(time.RFC3339), "days_remaining", daysRemaining,
		"due_at", timing.DueAt.Format(time.RFC3339), "remaining", timing.Remaining.Round(time.Second).String())

	if !timing.Due {
		roslog.D("Certificate not yet due for renewal", "days_remaining", daysRemaining, "due_at", timing.DueAt.Format(time.RFC3339))
		return false, nil
	}

	if daysRemaining < 0 {
		roslog.W("Certificate has already expired", nil, "expired_days_ago", -daysRemaining)
	} else {
		roslog.I("Certificate expiring soon, initiating auto-renewal", "days_remaining", daysRemaining)
	}

	// Perform auto-renewal
	if err := performAutoRenewal(); err != nil {
		return false, fmt.Errorf("auto-renewal failed: %w", err)
	}

	return true, nil
}

// performAutoRenewal handles the automatic certificate renewal process
func performAutoRenewal() error {
	roslog.I("Starting automatic certificate renewal")

	// Step 1: Backup existing certificates
	backupDir, err := backupCertificates()
	if err != nil {
		roslog.E("Failed to backup certificates during auto-renewal", err)
		return fmt.Errorf("backup failed: %w", err)
	}
	roslog.I("Certificates backed up", "backup_dir", backupDir)

	// Step 2: Connect to Nodeward and request new certificate
	roslog.I("Connecting to Nodeward for certificate renewal")
	c, _, cancel, conn, err := backend.NodewardL2Sec()
	if err != nil {
		roslog.E("Failed to connect to Nodeward for certificate auto-renewal", err)
		return fmt.Errorf("connect failed: %w", err)
	}
	defer cancel()
	defer conn.Close()

	// Create context with timeout for the renewal operation
	ctx, cancelCtx := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCtx()

	// Step 3: Call RegenerateCertificate RPC
	roslog.I("Requesting new certificate from Nodeward")
	paths := registernode.DefaultCredPaths()
	response, err := requestCertificate(ctx, c, paths, time.Now)
	if err != nil {
		roslog.E("Failed to regenerate certificate", err)
		cleanupBackup(backupDir)
		return fmt.Errorf("regenerate certificate RPC failed: %w", err)
	}
	roslog.I("New certificate received from Nodeward")

	// Step 4: Write new certificates to disk
	if err := writeCertificates(response, paths); err != nil {
		roslog.E("Failed to write new certificates", err)
		// Attempt to restore from backup
		if restoreErr := restoreCertificates(backupDir); restoreErr != nil {
			roslog.E("Failed to restore certificates from backup", restoreErr, "backup_dir", backupDir)
			return fmt.Errorf("write failed and restore failed: %w (restore error: %v)", err, restoreErr)
		}
		roslog.I("Certificates restored from backup after write failure")
		return fmt.Errorf("write certificates failed: %w", err)
	}
	roslog.I("New certificates written to disk")

	// Step 5: Test new certificates
	if err := testNewCertificates(); err != nil {
		roslog.E("New certificate test failed", err)
		// Attempt to restore from backup
		if restoreErr := restoreCertificates(backupDir); restoreErr != nil {
			roslog.E("Failed to restore certificates from backup", restoreErr, "backup_dir", backupDir)
			return fmt.Errorf("test failed and restore failed: %w (restore error: %v)", err, restoreErr)
		}
		roslog.I("Certificates restored from backup after test failure")
		return fmt.Errorf("certificate test failed: %w", err)
	}
	roslog.I("New certificates validated successfully")

	// Step 6: Cleanup backup
	cleanupBackup(backupDir)

	// Log the renewal to Nodeward
	if err := backend.AddNodelog(3, "CertificateAutoRenewal", "Certificate automatically renewed"); err != nil {
		roslog.W("Failed to log certificate renewal to Nodeward", err)
	}

	roslog.I("Automatic certificate renewal completed successfully")
	return nil
}
