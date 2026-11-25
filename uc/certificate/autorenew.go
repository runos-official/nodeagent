package certificate

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/uc/certpolicy"
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
