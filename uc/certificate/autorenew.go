package certificate

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certpolicy"
	"github.com/runos-official/nodeagent/uc/certstore"
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

// performAutoRenewal renews the certificate through the shared renewal
// function. A certificate that another process already renewed is a
// success.
func performAutoRenewal() error {
	roslog.I("Starting automatic certificate renewal")
	d := defaultDeps("CertificateAutoRenewal", "Certificate automatically renewed")
	// The serial of the certificate that the caller just evaluated.
	d.ExpectedSerial, _ = certstore.Open(d.Paths).HeldSerial()
	res, err := renewOnce(context.Background(), d)
	if err != nil {
		roslog.E("Automatic certificate renewal failed", err)
		return err
	}
	roslog.I("Automatic certificate renewal completed", "status", res.Status, "serial", res.Serial,
		"not_after", res.NotAfter.Format(time.RFC3339))
	return nil
}
