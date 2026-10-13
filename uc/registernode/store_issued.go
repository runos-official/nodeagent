package registernode

import (
	"fmt"
	"time"

	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/uc/csrcert"
)

// CredPaths names the three files that hold the node's mTLS credentials.
type CredPaths struct {
	Cert string
	Key  string
	CA   string
}

// DefaultCredPaths returns the configured credential paths.
func DefaultCredPaths() CredPaths {
	return CredPaths{
		Cert: config.GetPublicKeyPath(),
		Key:  config.GetPrivateKeyPath(),
		CA:   config.GetCACertPath(),
	}
}

// StoreCredentials checks a minting response and writes it (FPL315). The
// response content decides the path: a certificate means the local key is
// stored with it; a key with no certificate means an old nodeward, and the
// legacy fields are stored as before. A refused response writes nothing.
func StoreCredentials(key *csrcert.Key, resp csrcert.Response, p CredPaths) (csrcert.Outcome, error) {
	outcome, err := csrcert.Resolve(key, resp, time.Now())
	if err != nil {
		return 0, err
	}
	return outcome, WriteCredentials(outcome, key, resp, p)
}

// WriteCredentials writes a response that csrcert.Resolve already accepted.
func WriteCredentials(outcome csrcert.Outcome, key *csrcert.Key, resp csrcert.Response, p CredPaths) error {
	if err := StorePublicKey(resp.CACertPEM, p.CA); err != nil {
		return fmt.Errorf("could not store CA certificate: %w", err)
	}
	switch outcome {
	case csrcert.OutcomeCertificate:
		if err := StorePublicKey(resp.CertificatePEM, p.Cert); err != nil {
			return fmt.Errorf("could not store certificate: %w", err)
		}
		if err := csrcert.WriteKeyFile(key, p.Key); err != nil {
			return fmt.Errorf("could not store private key: %w", err)
		}
	case csrcert.OutcomeLegacy:
		if err := StorePublicKey(resp.PublicKey, p.Cert); err != nil {
			return fmt.Errorf("could not store certificate: %w", err)
		}
		if err := StorePrivateKey(resp.PrivateKey, p.Key); err != nil {
			return fmt.Errorf("could not store private key: %w", err)
		}
	default:
		return fmt.Errorf("no credentials to write (outcome %v)", outcome)
	}
	return nil
}
