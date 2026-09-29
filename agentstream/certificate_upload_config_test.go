package agentstream

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/runos-official/nodeagent/l2sec"
)

// A second control plane needs the cluster configuration and a stable key.
// kubeadm refuses --config together with --certificate-key, so the key must
// be supplied in a private temporary InitConfiguration instead.
func TestGetCertKeyUsesClusterKubeadmConfigWithKey(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "kubeadm")
	contents := `#!/bin/sh
config=''
for arg in "$@"; do
  case "$arg" in
    --certificate-key) exit 1 ;;
    --config=*) config=${arg#--config=} ;;
  esac
done
test -n "$config" || exit 1
grep -q '^kind: InitConfiguration$' "$config" || exit 1
grep -Eq '^certificateKey: [0-9a-f]{64}$' "$config" || exit 1
grep -q '^kind: ClusterConfiguration$' "$config" || exit 1
printf '%s' "$config" > "$CHECK_PATH_FILE"
exit 0
`
	if err := os.WriteFile(shim, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	checkPathFile := filepath.Join(dir, "config-path.txt")
	t.Setenv("CHECK_PATH_FILE", checkPathFile)
	oldConfig := kubeadmConfigPath
	kubeadmConfigPath = filepath.Join(dir, "kubeadm-config.yaml")
	t.Cleanup(func() { kubeadmConfigPath = oldConfig })
	if err := os.WriteFile(kubeadmConfigPath, []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n"), 0600); err != nil {
		t.Fatal(err)
	}
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
	path, err := os.ReadFile(checkPathFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(path)); !os.IsNotExist(err) {
		t.Fatalf("temporary key-bearing kubeadm config was not removed: %v", err)
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
