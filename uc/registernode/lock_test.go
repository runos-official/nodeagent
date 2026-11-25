package registernode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/uc/certstore"
	"github.com/runos-official/nodeagent/uc/csrcert"
	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
)

// TestRegistrationWaitsForRenewalLock: a registration writes no credential
// file while a renewal holds the lock, and writes them once it is free.
func TestRegistrationWaitsForRenewalLock(t *testing.T) {
	ca := csrtest.NewCA(t)
	dir := t.TempDir()
	p := CredPaths{Cert: filepath.Join(dir, "mtls.crt"), Key: filepath.Join(dir, "mtls.key"), CA: filepath.Join(dir, "ca.crt")}
	keyPEM, certPEM := ca.LegacyPair(t)
	resp := csrcert.Response{PrivateKey: keyPEM, PublicKey: certPEM, CACertPEM: ca.PEM}

	old := registrationLockWait
	registrationLockWait = 300 * time.Millisecond
	t.Cleanup(func() { registrationLockWait = old })

	release, err := certstore.AcquireRenewalLock(context.Background(), dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = WriteCredentials(csrcert.OutcomeLegacy, nil, resp, p)
	if !errors.Is(err, certstore.ErrRenewalInProgress) {
		t.Fatalf("want renewal already in progress, got %v", err)
	}
	for _, f := range []string{p.Cert, p.Key, p.CA} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was written while the renewal lock was held", f)
		}
	}
	release()

	if err := WriteCredentials(csrcert.OutcomeLegacy, nil, resp, p); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p.Cert); string(got) == "" {
		t.Fatal("certificate not written after the lock was free")
	}
}
