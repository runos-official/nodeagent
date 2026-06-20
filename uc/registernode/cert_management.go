package registernode

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"github.com/runos-official/nodeagent/roslog"
	"os"
)

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
	// Create a new file
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

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
		roslog.I("Certificate contents", "contents", contents)
		roslog.E("failed to load client cert", err)
		return err
	}

	return writeCertificateToFile(cert, path)
}
