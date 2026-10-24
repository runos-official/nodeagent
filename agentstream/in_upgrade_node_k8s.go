package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

const (
	// UpgradeNodeK8sRequestType is the instruction type that upgrades Kubernetes on this node.
	UpgradeNodeK8sRequestType = "UPGRADE_NODE_K8S"
	// UpgradeNodeK8sResponseType is the response type for a node Kubernetes upgrade.
	UpgradeNodeK8sResponseType = "UPGRADE_NODE_K8S_RESPONSE"
	// UpgradeNodeK8sLogType is the message type carrying streamed upgrade log lines.
	UpgradeNodeK8sLogType = "UpgradeNodeK8sLog"
)

// Command runners for the upgrade. Tests replace them. upgradeRunShell is
// only for constant commands and for apt-get, which keeps its hang retry; every
// request value it carries is validated and quoted. Everything else runs
// without a shell.
var (
	upgradeRunShell   = commons.ExecuteCommandGetResponse2
	upgradeRunArgv    = commons.ExecuteArgvGetResponse
	upgradeStreamArgv = commons.ExecuteArgvStreaming
	upgradeWriteFile  = os.WriteFile
	upgradeHostname   = commons.GetHostname
	upgradeSleep      = time.Sleep
)

// kubernetesAptSourcePath is the apt source list the upgrade writes.
const kubernetesAptSourcePath = "/etc/apt/sources.list.d/kubernetes.list"

type upgradeNodeK8sRequest struct {
	TargetKubernetesVersion string `json:"targetKubernetesVersion"`
	TargetContainerdVersion string `json:"targetContainerdVersion"`
	TargetHelmVersion       string `json:"targetHelmVersion"`
	KubeRepoVersion         string `json:"kubeRepoVersion"`
	IsFirstCp               bool   `json:"isFirstCp"`
}

type upgradeNodeK8sResponse struct {
	Success         bool   `json:"success"`
	Message         string `json:"message"`
	PreviousVersion string `json:"previousVersion"`
	NewVersion      string `json:"newVersion"`
}

type upgradeNodeK8sLog struct {
	Message  string `json:"message"`
	Severity int    `json:"severity"`
}

// HandleUpgradeNodeK8s performs an in-place Kubernetes upgrade on this node.
// It trusts the caller (Conductor) to have already verified cluster health and
// drained / cordoned the node as appropriate — this handler only executes the
// local upgrade work.
func HandleUpgradeNodeK8s(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	return handleUpgradeNodeK8s(instruction, NewProgressTagger(instruction))
}

func handleUpgradeNodeK8s(instruction *pb.ToNodeAgent, progress *ProgressTagger) (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleUpgradeNodeK8s")
	tag := instruction.Tag

	jsonData, err := base64.StdEncoding.DecodeString(instruction.JsonB64)
	if err != nil {
		return failUpgrade(progress, "", tag, "failed to decode payload: %v", err), nil
	}

	var req upgradeNodeK8sRequest
	if err := json.Unmarshal(jsonData, &req); err != nil {
		return failUpgrade(progress, "", tag, "failed to parse payload: %v", err), nil
	}
	// Refuse a malformed version before any command runs. Each value goes into
	// a command that runs as root.
	if err := validateUpgradeNodeK8sRequest(req); err != nil {
		return failUpgrade(progress, "", tag, "refusing the upgrade: %v", err), nil
	}

	previous := getCurrentKubeletVersion()
	sendUpgradeLog(progress, fmt.Sprintf("Starting K8s upgrade %s → %s (isFirstCp=%t)",
		previous, req.TargetKubernetesVersion, req.IsFirstCp), 1)

	hostname, err := upgradeHostname()
	if err != nil {
		return failUpgrade(progress, previous, tag, "failed to get hostname: %v", err), nil
	}

	// Pre-flight: confirm pkgs.k8s.io is reachable BEFORE we touch any system
	// state. Omitting -f means curl returns success for any HTTP response (even a
	// 404) and fails only on DNS / TLS / connection problems, so this isolates a
	// connectivity fault. The retry absorbs the common "DNS not yet up after
	// un-cordon" race; if it still fails we abort loud and obvious with no changes
	// made, rather than dying half-way through repo configuration.
	sendUpgradeLog(progress, "Pre-flight: verifying pkgs.k8s.io is reachable before making changes", 1)
	if out, err := runNetworkCommandWithRetry(progress, "pkgs.k8s.io reachability", func() (string, error) {
		return upgradeRunArgv("curl", "-sS", "-o", "/dev/null", "https://pkgs.k8s.io")
	}); err != nil {
		return failUpgrade(progress, previous, tag, "pre-flight connectivity check failed: %s (aborting before any changes) (%s)",
			classifyNetworkError(out, err), tailOutput(out, 500)), nil
	}

	sendUpgradeLog(progress, fmt.Sprintf("Configuring K8s apt repo for %s", req.KubeRepoVersion), 1)
	if out, err := upgradeRunArgv("mkdir", "-p", "/etc/apt/keyrings"); err != nil {
		return failUpgrade(progress, previous, tag, "mkdir keyrings failed: %v (%s)", err, tailOutput(out, 500)), nil
	}
	// Fetch the key to a temp file (with retry), THEN dearmor it as a separate
	// step. The original single `curl ... | gpg` pipe reported gpg's exit code (2)
	// and "no valid OpenPGP data" while burying curl's real "could not resolve
	// host", and a single transient failure aborted the whole upgrade. Splitting
	// the steps lets us retry only the network fetch and report the true cause.
	keyURL := fmt.Sprintf("https://pkgs.k8s.io/core:/stable:/%s/deb/Release.key", req.KubeRepoVersion)
	if out, err := runNetworkCommandWithRetry(progress, "apt keyring fetch", func() (string, error) {
		return upgradeRunArgv("curl", "-fsSL", keyURL, "-o", "/tmp/kubernetes-release.key")
	}); err != nil {
		return failUpgrade(progress, previous, tag, "apt keyring fetch failed: %s (%s)", classifyNetworkError(out, err), tailOutput(out, 500)), nil
	}
	if out, err := upgradeRunArgv("gpg", "--batch", "--yes", "--dearmor",
		"-o", "/etc/apt/keyrings/kubernetes-apt-keyring.gpg", "/tmp/kubernetes-release.key"); err != nil {
		return failUpgrade(progress, previous, tag, "apt keyring dearmor failed: %s (%s)", classifyNetworkError(out, err), tailOutput(out, 500)), nil
	}
	// Go writes the source list; the old `echo ... > file` needed a shell.
	source := fmt.Sprintf(
		"deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/%s/deb/ /\n",
		req.KubeRepoVersion)
	if err := upgradeWriteFile(kubernetesAptSourcePath, []byte(source), 0o644); err != nil {
		return failUpgrade(progress, previous, tag, "writing kubernetes.list failed: %v", err), nil
	}
	if out, err := upgradeRunShell("apt-get update"); err != nil {
		return failUpgrade(progress, previous, tag, "apt-get update failed: %v (%s)", err, tailOutput(out, 500)), nil
	}

	sendUpgradeLog(progress, fmt.Sprintf("Installing kubeadm %s", req.TargetKubernetesVersion), 1)
	if out, err := upgradeRunShell("apt-mark unhold kubeadm"); err != nil {
		return failUpgrade(progress, previous, tag, "apt-mark unhold kubeadm failed: %v (%s)", err, tailOutput(out, 500)), nil
	}
	kubeadmCmd := "apt-get install -y " + shellQuote("kubeadm="+req.TargetKubernetesVersion+"-*")
	if out, err := upgradeRunShell(kubeadmCmd); err != nil {
		return failUpgrade(progress, previous, tag, "kubeadm install failed: %v (%s)", err, tailOutput(out, 500)), nil
	}
	if out, err := upgradeRunShell("apt-mark hold kubeadm"); err != nil {
		return failUpgrade(progress, previous, tag, "apt-mark hold kubeadm failed: %v (%s)", err, tailOutput(out, 500)), nil
	}

	forwardKubeadmProgress := func(line string) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") || strings.Contains(trimmed, "SUCCESS!") {
			sendUpgradeLog(progress, trimmed, 1)
		}
	}

	if req.IsFirstCp {
		upgradeCmd := fmt.Sprintf("kubeadm upgrade apply v%s --yes", req.TargetKubernetesVersion)
		sendUpgradeLog(progress, "Running kubeadm upgrade apply", 1)
		out, err := upgradeStreamArgv(forwardKubeadmProgress, "kubeadm", "upgrade", "apply", "v"+req.TargetKubernetesVersion, "--yes")
		if err != nil {
			// `kubeadm upgrade apply` can exit non-zero due to transient gRPC disruption
			// while it's restarting its own control-plane static pods — even though the
			// upgrade has in fact been applied. Verify actual server version before
			// treating this as a failure.
			sendUpgradeLog(progress, fmt.Sprintf("%s exited non-zero (%v); verifying cluster state", upgradeCmd, err), 2)
			if !verifyServerAtVersion(req.TargetKubernetesVersion, 2*time.Minute) {
				return failUpgrade(progress, previous, tag, "%s failed: %v (%s)", upgradeCmd, err, tailOutput(out, 500)), nil
			}
			sendUpgradeLog(progress, "Control plane reached target version despite kubeadm exit error; continuing", 1)
		}
	} else {
		upgradeCmd := "kubeadm upgrade node"
		sendUpgradeLog(progress, "Running kubeadm upgrade node", 1)
		if out, err := upgradeStreamArgv(forwardKubeadmProgress, "kubeadm", "upgrade", "node"); err != nil {
			return failUpgrade(progress, previous, tag, "%s failed: %v (%s)", upgradeCmd, err, tailOutput(out, 500)), nil
		}
	}

	sendUpgradeLog(progress, fmt.Sprintf("Installing kubelet and kubectl %s", req.TargetKubernetesVersion), 1)
	if out, err := upgradeRunShell("apt-mark unhold kubelet kubectl"); err != nil {
		return failUpgrade(progress, previous, tag, "apt-mark unhold kubelet kubectl failed: %v (%s)", err, tailOutput(out, 500)), nil
	}
	kubeletCmd := "apt-get install -y " + shellQuote("kubelet="+req.TargetKubernetesVersion+"-*") +
		" " + shellQuote("kubectl="+req.TargetKubernetesVersion+"-*")
	if out, err := upgradeRunShell(kubeletCmd); err != nil {
		return failUpgrade(progress, previous, tag, "kubelet/kubectl install failed: %v (%s)", err, tailOutput(out, 500)), nil
	}
	if out, err := upgradeRunShell("apt-mark hold kubelet kubectl"); err != nil {
		return failUpgrade(progress, previous, tag, "apt-mark hold kubelet kubectl failed: %v (%s)", err, tailOutput(out, 500)), nil
	}

	currentContainerd := getCurrentContainerdVersion()
	if !strings.HasPrefix(currentContainerd, req.TargetContainerdVersion) {
		sendUpgradeLog(progress, fmt.Sprintf("Upgrading containerd %s → %s", currentContainerd, req.TargetContainerdVersion), 1)
		cmd := "apt-get install -y " + shellQuote("containerd="+req.TargetContainerdVersion+"-*")
		if out, err := upgradeRunShell(cmd); err != nil {
			return failUpgrade(progress, previous, tag, "containerd upgrade failed: %v (%s)", err, tailOutput(out, 500)), nil
		}
	} else {
		sendUpgradeLog(progress, fmt.Sprintf("containerd already at %s", req.TargetContainerdVersion), 1)
	}

	currentHelm := getCurrentHelmVersion()
	if currentHelm != req.TargetHelmVersion {
		sendUpgradeLog(progress, fmt.Sprintf("Upgrading helm %s → %s", currentHelm, req.TargetHelmVersion), 1)
		// The pipe needs a shell. The version is validated and quoted.
		cmd := "curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | DESIRED_VERSION=" +
			shellQuote("v"+req.TargetHelmVersion) + " bash"
		if out, err := runNetworkCommandWithRetry(progress, "helm install", func() (string, error) {
			return upgradeRunShell(cmd)
		}); err != nil {
			return failUpgrade(progress, previous, tag, "helm upgrade failed: %s (%s)", classifyNetworkError(out, err), tailOutput(out, 500)), nil
		}
	} else {
		sendUpgradeLog(progress, fmt.Sprintf("helm already at %s", req.TargetHelmVersion), 1)
	}

	sendUpgradeLog(progress, "Reloading systemd and restarting kubelet", 1)
	if out, err := upgradeRunShell("systemctl daemon-reload && systemctl restart kubelet"); err != nil {
		return failUpgrade(progress, previous, tag, "kubelet restart failed: %v (%s)", err, tailOutput(out, 500)), nil
	}

	sendUpgradeLog(progress, "Waiting for node to report target kubelet version and Ready", 1)
	if err := waitForNodeAtVersion(hostname, req.TargetKubernetesVersion, 5*time.Minute); err != nil {
		return failUpgrade(progress, previous, tag, "%v", err), nil
	}

	sendUpgradeLog(progress, fmt.Sprintf("Upgrade completed successfully: %s → %s", previous, req.TargetKubernetesVersion), 1)
	return buildUpgradeResponse(true, "Upgrade completed successfully", previous, req.TargetKubernetesVersion, tag), nil
}

// networkRetryDelays is the backoff schedule for outbound-network steps during
// an upgrade (the apt keyring fetch and the helm install). A momentary DNS or
// connectivity blip (common right after a node is un-cordoned and its
// networking is still settling) must not abort the whole upgrade. Each entry is
// the wait after a failed attempt, so the command is tried len()+1 times total.
var networkRetryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second}

// runNetworkCommandWithRetry runs a command that depends on outbound network
// access, retrying with backoff on failure. label is used only for log lines.
// It returns the output and error of the final attempt.
func runNetworkCommandWithRetry(progress *ProgressTagger, label string, run func() (string, error)) (string, error) {
	attempts := len(networkRetryDelays) + 1
	var out string
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		out, err = run()
		if err == nil {
			return out, nil
		}
		if attempt < attempts {
			delay := networkRetryDelays[attempt-1]
			sendUpgradeLog(progress, fmt.Sprintf("%s failed (attempt %d/%d): %s; retrying in %s",
				label, attempt, attempts, classifyNetworkError(out, err), delay), 2)
			upgradeSleep(delay)
		}
	}
	return out, err
}

// classifyNetworkError turns the raw output + error of a failed network command
// into a short, actionable description so operators can tell a DNS failure apart
// from a TLS error, a refused connection, a timeout, or a genuine content
// problem, instead of an opaque "exit status N". curl's -S flag writes these
// signatures to stderr, which ExecuteCommandGetResponse2 captures via
// CombinedOutput, so we can match on them here.
func classifyNetworkError(output string, err error) string {
	o := strings.ToLower(output)
	switch {
	case strings.Contains(o, "could not resolve host"):
		return "DNS resolution failed (could not resolve host); node DNS may not be ready"
	case strings.Contains(o, "connection refused"):
		return "connection refused"
	case strings.Contains(o, "timed out"), strings.Contains(o, "operation timed out"):
		return "connection timed out"
	case strings.Contains(o, "network is unreachable"), strings.Contains(o, "couldn't connect"):
		return "network unreachable"
	case strings.Contains(o, "ssl"), strings.Contains(o, "tls"), strings.Contains(o, "certificate"):
		return "TLS/certificate error"
	case strings.Contains(o, "no valid openpgp data"):
		return "fetched content is not a valid OpenPGP key (upstream returned an error page or empty body)"
	default:
		return fmt.Sprintf("%v", err)
	}
}

const upgradeLogMaxBytes = 4096

func sendUpgradeLog(progress *ProgressTagger, message string, severity int) {
	switch severity {
	case 3:
		roslog.E("upgrade: "+message, nil)
	case 2:
		roslog.W("upgrade: "+message, nil)
	default:
		roslog.I("upgrade: " + message)
	}
	if len(message) > upgradeLogMaxBytes {
		message = message[:upgradeLogMaxBytes] + "\n[... log message truncated ...]"
	}
	payload := upgradeNodeK8sLog{Message: message, Severity: severity}
	data, err := json.Marshal(payload)
	if err != nil {
		roslog.E("failed to marshal upgrade log", err)
		return
	}
	if err := progress.Send(&pb.FromNodeAgent{
		JsonB64: base64.StdEncoding.EncodeToString(data),
		Type:    UpgradeNodeK8sLogType,
	}); err != nil {
		roslog.E("failed to send upgrade log", err, "message", message)
	}
}

func buildUpgradeResponse(success bool, message, prev, newV, tag string) *pb.FromNodeAgent {
	resp := upgradeNodeK8sResponse{
		Success:         success,
		Message:         message,
		PreviousVersion: prev,
		NewVersion:      newV,
	}
	data, _ := json.Marshal(resp)
	return &pb.FromNodeAgent{
		JsonB64: base64.StdEncoding.EncodeToString(data),
		Type:    UpgradeNodeK8sResponseType,
		Tag:     tag,
	}
}

func failUpgrade(progress *ProgressTagger, prev, tag, format string, args ...any) *pb.FromNodeAgent {
	msg := fmt.Sprintf(format, args...)
	sendUpgradeLog(progress, msg, 3)
	return buildUpgradeResponse(false, msg, prev, "", tag)
}

func getCurrentKubeletVersion() string {
	out, err := upgradeRunShell("kubelet --version")
	if err != nil {
		return ""
	}
	parts := strings.Fields(strings.TrimSpace(out))
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimPrefix(parts[1], "v")
}

func getCurrentContainerdVersion() string {
	out, err := upgradeRunShell(`dpkg-query -W -f='${Version}' containerd`)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.Trim(out, "'"))
}

func getCurrentHelmVersion() string {
	out, err := upgradeRunShell("helm version --short --client")
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(out)
	v = strings.TrimPrefix(v, "v")
	if idx := strings.Index(v, "+"); idx > 0 {
		v = v[:idx]
	}
	return v
}

func verifyServerAtVersion(target string, timeout time.Duration) bool {
	expected := "v" + target
	cmd := `kubectl version -o jsonpath='{.serverVersion.gitVersion}'`
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := upgradeRunShell(cmd)
		if err == nil {
			v := strings.TrimSpace(strings.Trim(out, "'"))
			if v == expected {
				return true
			}
		}
		upgradeSleep(5 * time.Second)
	}
	return false
}

// waitForNodeAtVersion polls until the node reports BOTH the target kubelet
// version AND a Ready=True condition. Checking only Ready is unreliable right
// after `systemctl restart kubelet`: systemctl returns immediately, the node
// lease hasn't expired yet, so the cluster still reports Ready=True for the
// old kubelet for ~30–40s. The kubeletVersion field only flips once the new
// kubelet has actually come up and registered, so that's the authoritative
// signal that the upgrade has taken effect on this node.
func waitForNodeAtVersion(hostname, targetVersion string, timeout time.Duration) error {
	expectedVersion := "v" + targetVersion
	versionArgs := []string{"get", "node", hostname, "-o", "jsonpath={.status.nodeInfo.kubeletVersion}"}
	readyArgs := []string{"get", "node", hostname, "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`}
	deadline := time.Now().Add(timeout)
	var lastVersion, lastReady string
	for time.Now().Before(deadline) {
		if out, err := upgradeRunArgv("kubectl", versionArgs...); err == nil {
			lastVersion = strings.TrimSpace(strings.Trim(out, "'"))
		}
		if out, err := upgradeRunArgv("kubectl", readyArgs...); err == nil {
			lastReady = strings.TrimSpace(strings.Trim(out, "'"))
		}
		if lastVersion == expectedVersion && lastReady == "True" {
			return nil
		}
		upgradeSleep(5 * time.Second)
	}
	return fmt.Errorf("node %s did not reach kubeletVersion=%s Ready=True within %s (last seen: version=%q ready=%q)",
		hostname, expectedVersion, timeout, lastVersion, lastReady)
}

func tailOutput(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
