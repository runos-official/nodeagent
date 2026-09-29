package agentstream

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/runos-official/nodeagent/l2sec"
)

// A second control plane must not fall back to kubeadm's default configuration
// while its first control plane uploads the join certificates.
func TestGetCertKeyUsesClusterKubeadmConfig(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "kubeadm")
	contents := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "--config=/etc/kubernetes/kubeadm-config.yaml" ]; then
    exit 0
  fi
done
echo 'cluster kubeadm config was not supplied' >&2
exit 1
`
	if err := os.WriteFile(shim, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := certKeyPath
	certKeyPath = filepath.Join(dir, "cert-upload-key.txt")
	t.Cleanup(func() { certKeyPath = old })

	key := getCertKey()
	if len(key) != 64 {
		t.Fatalf("certificate upload failed or returned an invalid key (length %d)", len(key))
	}
	stored, err := os.ReadFile(certKeyPath)
	if err != nil || strings.TrimSpace(string(stored)) != key {
		t.Fatalf("certificate key was not cached after upload: %v", err)
	}
}

func TestControlPlaneJoinDoesNotReturnAnEmptyCertificateKey(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "kubeadm")
	contents := `#!/bin/sh
if [ "$1" = "token" ]; then
  echo 'kubeadm join example.invalid:6443 --token abcd.efgh --discovery-token-ca-cert-hash sha256:1234'
  exit 0
fi
echo 'certificate upload failed' >&2
exit 1
`
	if err := os.WriteFile(shim, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := certKeyPath
	certKeyPath = filepath.Join(dir, "cert-upload-key.txt")
	t.Cleanup(func() { certKeyPath = old })

	instruction := &pb.ToNodeAgent{JsonB64: base64.StdEncoding.EncodeToString([]byte(`{"asCp":true}`))}
	response, err := HandleGetClusterJoinCommand(instruction)
	if err == nil || response != nil {
		t.Fatalf("failed certificate upload must not return a join command, response=%v error=%v", response, err)
	}
}
