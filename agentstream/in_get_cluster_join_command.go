package agentstream

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

const (
	// GetClusterJoinCommandRequestType is the instruction type requesting a kubeadm join command.
	GetClusterJoinCommandRequestType = "GET_CLUSTER_JOIN_CMD"
	// GetClusterJoinCommandResponseType is the response type carrying the join command.
	GetClusterJoinCommandResponseType = "CLUSTER_JOIN_CMD"
)

type getClusterJoinCommandRequest struct {
	AsCp bool `json:"asCp"`
}

type getClusterJoinCommandResponse struct {
	JoinCmd       string `json:"joinCmd"`
	EncryptionKey string `json:"encryptionKey,omitempty"`
}

// HandleGetClusterJoinCommand returns a kubeadm join command for the requesting
// node, including the control-plane certificate key when joining as a control plane.
func HandleGetClusterJoinCommand(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleGetClusterJoinCommand")
	jsonData, err := base64.StdEncoding.DecodeString(instruction.JsonB64)
	if err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	var request getClusterJoinCommandRequest
	if err := json.Unmarshal(jsonData, &request); err != nil {
		roslog.E("Error unmarshalling JSON payload", err)
		return nil, err
	}

	response := getClusterJoinCommandResponse{
		//JoinCmd: joinCmd,
	}

	//var joinCmd string
	if request.AsCp {
		response.JoinCmd = getClusterJoinCommandAsCp()
		response.EncryptionKey = getEncryptionKey()
		//joinCmd = getClusterJoinCommandAsCp()
	} else {
		response.JoinCmd = getMinJoinCommand()
		//joinCmd = getMinJoinCommand()
	}
	//response := getClusterJoinCommandResponse{
	//	JoinCmd: joinCmd,
	//}

	responseJson, err := json.Marshal(response)
	if err != nil {
		roslog.E("Error marshalling response JSON", err)
		return nil, err
	}
	responseJsonB64 := base64.StdEncoding.EncodeToString(responseJson)

	return &pb.FromNodeAgent{
		JsonB64: responseJsonB64,
		Type:    GetClusterJoinCommandResponseType,
	}, nil
}

// GetClusterJoinCommandAsCp
// kubeadm join <control-plane-endpoint>:6443 --token <token> --discovery-token-ca-cert-hash sha256:<hash> --control-plane --certificate-key 074ab7df6359cb2c21e6a6e10c255065b162c7332ba231eec33a7e18fbd77a10
func getClusterJoinCommandAsCp() string {
	return getMinJoinCommand() + " --control-plane --certificate-key " + getCertKey()
}

// GetClusterJoinCommandAsWorker
// kubeadm join cp:6443 --token vpns94.v31cyd1flfmgi7le --discovery-token-ca-cert-hash sha256:41e2312c18834ff4393a0f31dda148c84aa36ba508479bcae262bc621e14629f
func getClusterJoinCommandAsWorker() string {
	return getMinJoinCommand()
}

func getMinJoinCommand() string {
	cmd := "kubeadm token create --print-join-command"
	out, err := exec.Command("/bin/sh", "-c", cmd).Output()
	if err != nil {
		roslog.E("Error getting machine id", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

func getEncryptionKey() string {
	data, err := commons.ExecuteDirectCommandGetResponse("cat", false, "/etc/kubernetes/enc/key.txt")
	if err != nil {
		roslog.E("Error getting admin.conf", err)
		return ""
	}
	return *data
}

// certKeyPath caches the key this node last uploaded the cluster certificates with. A variable
// rather than a constant so the tests can point it at a temporary directory.
var certKeyPath = "/etc/kubernetes/enc/cert-upload-key.txt"

// certKeyReuseWindow is how long a cached key is reused. Comfortably inside kubeadm's own two-hour
// expiry on the uploaded Secret, so a key handed out at the start of the window is still valid
// when the joiner uses it, and short enough that a key does not live on the disk indefinitely.
const certKeyReuseWindow = 60 * time.Minute

// certKeyMu serialises the upload on THIS node, so two joins arriving together cannot interleave
// the read-generate-upload sequence and end up with different keys.
var certKeyMu sync.Mutex

// getCertKey uploads the cluster certificates and returns the key needed to download them.
//
// THE KEY IS STABLE FOR A WINDOW, and that is the fix for the control-plane join race (goal 23,
// F5). `kubeadm init phase upload-certs --upload-certs` generates a FRESH key on every call and
// re-encrypts the kubeadm-certs Secret with it, so a second control-plane join starting while the
// first was still downloading invalidated the key the first had been handed:
//
//	error execution phase control-plane-prepare/download-certs: error downloading certs:
//	  error decoding secret data with provided key: cipher: message authentication failed
//
// Reproduced on one cluster 2026-08-12 and again on a second cluster the same day, forty seconds
// apart. Passing an explicit --certificate-key makes the upload IDEMPOTENT: two joins are handed
// the same key, the second upload re-encrypts with the key the first is already using, and both
// decrypt. That removes the race rather than narrowing its window, which is what a check alone
// can do.
func getCertKey() string {
	certKeyMu.Lock()
	defer certKeyMu.Unlock()

	key := readCachedCertKey()
	if key == "" {
		generated, err := newCertKey()
		if err != nil {
			roslog.E("Error generating certificate key", err)
			return ""
		}
		key = generated
	}

	// Re-uploaded every time, not only when the key is new: it refreshes the Secret's expiry and
	// restores it if something else has since overwritten it.
	cmd := "kubeadm init phase upload-certs --upload-certs --certificate-key " + key
	if _, err := exec.Command("/bin/sh", "-c", cmd).Output(); err != nil {
		roslog.E("Error uploading cluster certificates", err)
		return ""
	}

	if err := os.MkdirAll(filepath.Dir(certKeyPath), 0o700); err == nil {
		if err := os.WriteFile(certKeyPath, []byte(key), 0o600); err != nil {
			// Not fatal: the join can still proceed with the key in hand. It only means the NEXT
			// join generates a fresh one, which is the old behaviour.
			roslog.W("Could not cache the certificate key; the next join will generate a new one", err)
		}
	}

	return key
}

// readCachedCertKey returns the cached key while it is still inside the reuse window, or "".
func readCachedCertKey() string {
	info, err := os.Stat(certKeyPath)
	if err != nil || time.Since(info.ModTime()) > certKeyReuseWindow {
		return ""
	}
	body, err := os.ReadFile(certKeyPath)
	if err != nil {
		return ""
	}
	key := strings.TrimSpace(string(body))
	// 32 bytes as hex. Anything else is not a key kubeadm will accept, so treat it as absent
	// rather than passing it on and failing the join with a confusing error.
	if len(key) != 64 {
		return ""
	}
	return key
}

// newCertKey generates the 32-byte AES key kubeadm encrypts the certificate Secret with.
func newCertKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
