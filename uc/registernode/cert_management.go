package registernode

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"github.com/runos-official/nodeagent/roslog"
	"os"
)

// shortHash returns the first 8 hex chars of the sha256 of s, a non-reversible
// fingerprint usable in logs in place of secret/sensitive material.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func writeCertificateToFile(cert *x509.Certificate, filename string) error {
	// Create a new file
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	// Create a PEM block with the certificate
	pemBlock := &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	}

	// Write the PEM block to the file
	if err := pem.Encode(file, pemBlock); err != nil {
		return err
	}

	roslog.I("Certificate written", "filename", filename)
	return nil
}

func writePrivateKeyToFile(privateKey *rsa.PrivateKey, filename string) error {
	// Create (or truncate) the file root-only (0600). The private key must never
	// be world-readable.
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer file.Close()

	// Correct the mode even if the file already existed as 0644 (O_CREATE does
	// not change the perms of an existing file), so previously-issued keys are
	// tightened on the next write.
	if err := os.Chmod(filename, 0600); err != nil {
		return err
	}

	// Marshal the private key into its ASN.1 PKCS#1 DER-encoded form
	privateKeyBytes := x509.MarshalPKCS1PrivateKey(privateKey)

	// Create a PEM block with the private key
	pemBlock := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privateKeyBytes,
	}

	// Write the PEM block to the file
	if err := pem.Encode(file, pemBlock); err != nil {
		return err
	}

	roslog.I("Private key written", "filename", filename)
	return nil
}

func convertStringToCertificate(certPEM string) (*x509.Certificate, error) {
	// Decode the PEM block from the string
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, fmt.Errorf("failed to parse certificate PEM")
	}

	// Parse the certificate
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %v", err)
	}

	return cert, nil
}

func convertStringToPrivateKey(privKeyPEM string) (*rsa.PrivateKey, error) {
	// Decode the PEM block from the string
	block, _ := pem.Decode([]byte(privKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("failed to parse private key PEM")
	}

	// Parse the private key
	privKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %v", err)
	}

	return privKey, nil
}

// StorePrivateKey parses a PEM-encoded private key from contents and writes it
// to path.
func StorePrivateKey(contents string, path string) error {
	roslog.I("Storing private key", "path", path)
	cert, err := convertStringToPrivateKey(contents)
	if err != nil {
		roslog.E("failed to load client cert", err)
		return err
	}

	return writePrivateKeyToFile(cert, path)
}

// StorePublicKey parses a PEM-encoded certificate from contents and writes it
// to path.
func StorePublicKey(contents string, path string) error {
	roslog.I("Storing public key", "path", path)
	cert, err := convertStringToCertificate(contents)
	if err != nil {
		// Never log the PEM bytes (they may be key material on a misrouted
		// payload). Log only a non-reversible fingerprint + length for triage.
		roslog.E("failed to load client cert", err,
			"path", path, "bytes", len(contents), "sha256_8", shortHash(contents))
		return err
	}

	return writeCertificateToFile(cert, path)
}
