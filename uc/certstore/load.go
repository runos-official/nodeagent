package certstore

import (
	"crypto/tls"
	"os"
	"strings"
	"time"
)

const (
	loadReads = 5
	loadWait  = 100 * time.Millisecond
)

// loadReadHook runs at the start of each read of LoadKeyPair. Tests only.
var loadReadHook func()

// LoadKeyPair reads and parses the client key pair. An adoption renames the
// key before the certificate, so a reader can see a new key with the old
// certificate for a moment. On a key and certificate mismatch the function
// reads again, up to 5 reads, 100 ms apart. Every other error returns at
// once.
func LoadKeyPair(certPath, keyPath string) (tls.Certificate, error) {
	for read := 1; ; read++ {
		if loadReadHook != nil {
			loadReadHook()
		}
		certPEM, err := os.ReadFile(certPath)
		if err != nil {
			return tls.Certificate{}, err
		}
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			return tls.Certificate{}, err
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err == nil || !isKeyMismatch(err) || read >= loadReads {
			return pair, err
		}
		time.Sleep(loadWait)
	}
}

// isKeyMismatch matches the crypto/tls errors for a key that does not
// belong to the certificate.
func isKeyMismatch(err error) bool {
	return strings.Contains(err.Error(), "does not match public key")
}
