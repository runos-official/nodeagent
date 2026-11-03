package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
)

// The UPGRADE_NODE_K8S handler runs as root. Its version fields must never
// reach a shell as code (FCR 1648). These tests replace every command runner
// with a recorder, so no test runs a real command.

// upgradeRecorder records each command the upgrade runs and answers like a
// healthy node at the target version.
type upgradeRecorder struct {
	target   string
	commands []string
	files    map[string]string
}

func (r *upgradeRecorder) answer(command string) string {
	switch {
	case strings.Contains(command, "kubeletVersion"):
		return "v" + r.target
	case strings.Contains(command, `"Ready"`):
		return "True"
	case strings.HasPrefix(command, "sh: kubelet --version"):
		return "Kubernetes v1.35.4"
	case strings.Contains(command, "dpkg-query"):
		return "2.2.1-0ubuntu1"
	case strings.HasPrefix(command, "sh: helm version"):
		return "v3.22.0+g1234567"
	}
	return ""
}

func (r *upgradeRecorder) record(command string) (string, error) {
	r.commands = append(r.commands, command)
	return r.answer(command), nil
}

// install replaces the upgrade runners for the length of the test.
func (r *upgradeRecorder) install(t *testing.T) {
	t.Helper()
	r.files = map[string]string{}
	saved := saveUpgradeRunners()
	t.Cleanup(func() { restoreUpgradeRunners(saved) })
	installUpgradeRecorder(r)
}

func upgradeInstruction(t *testing.T, req upgradeNodeK8sRequest) *pb.ToNodeAgent {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return &pb.ToNodeAgent{Type: UpgradeNodeK8sRequestType, Tag: "t1", JsonB64: base64.StdEncoding.EncodeToString(data)}
}

func upgradeResult(t *testing.T, resp *pb.FromNodeAgent) upgradeNodeK8sResponse {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(resp.JsonB64)
	if err != nil {
		t.Fatal(err)
	}
	var out upgradeNodeK8sResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func validUpgradeRequest() upgradeNodeK8sRequest {
	return upgradeNodeK8sRequest{
		TargetKubernetesVersion: "1.35.9",
		TargetContainerdVersion: "2.2.1",
		TargetHelmVersion:       "3.22.0",
		KubeRepoVersion:         "v1.35",
		IsFirstCp:               true,
	}
}

var upgradeInjectionPayloads = []string{
	"$(id)",
	"`id`",
	"1.35.9;id",
	"1.35.9\nid",
	"1.35.9'id",
	`1.35.9"id`,
	`1.35.9\`,
	"1.35.9 id",
	"1.35.9|id",
	"1.35.9&id",
	"1.35.9$HOME",
	"",
}

func TestUpgradeNodeK8sRefusesMalformedVersionFields(t *testing.T) {
	fields := map[string]func(*upgradeNodeK8sRequest, string){
		"targetKubernetesVersion": func(r *upgradeNodeK8sRequest, v string) { r.TargetKubernetesVersion = v },
		"targetContainerdVersion": func(r *upgradeNodeK8sRequest, v string) { r.TargetContainerdVersion = v },
		"targetHelmVersion":       func(r *upgradeNodeK8sRequest, v string) { r.TargetHelmVersion = v },
		"kubeRepoVersion":         func(r *upgradeNodeK8sRequest, v string) { r.KubeRepoVersion = v },
	}
	for field, set := range fields {
		for _, payload := range upgradeInjectionPayloads {
			req := validUpgradeRequest()
			set(&req, payload)
			rec := &upgradeRecorder{target: req.TargetKubernetesVersion}
			rec.install(t)

			resp, err := handleUpgradeNodeK8s(upgradeInstruction(t, req), &ProgressTagger{})
			if err != nil {
				t.Fatalf("%s=%q: handler error: %v", field, payload, err)
			}
			if upgradeResult(t, resp).Success {
				t.Errorf("%s=%q: upgrade reported success, want refusal", field, payload)
			}
			if len(rec.commands) != 0 {
				t.Errorf("%s=%q: %d commands ran before the refusal, want 0; first: %q", field, payload, len(rec.commands), rec.commands[0])
			}
			if payload != "" {
				for _, c := range rec.commands {
					if strings.Contains(c, payload) {
						t.Errorf("%s=%q: the value reached a command: %q", field, payload, c)
					}
				}
			}
		}
	}
}

// The apt repo version must name the same minor as the target version.
func TestUpgradeNodeK8sRefusesARepoVersionForAnotherMinor(t *testing.T) {
	for _, repo := range []string{"v1.34", "1.35", "v1.35.9", "v1", "v1.3"} {
		rec := &upgradeRecorder{target: "1.35.9"}
		rec.install(t)
		req := validUpgradeRequest()
		req.KubeRepoVersion = repo
		resp, _ := handleUpgradeNodeK8s(upgradeInstruction(t, req), &ProgressTagger{})
		if upgradeResult(t, resp).Success || len(rec.commands) != 0 {
			t.Errorf("kubeRepoVersion=%q: want refusal before any command, got success=%v commands=%d",
				repo, upgradeResult(t, resp).Success, len(rec.commands))
		}
	}
}

// A valid request from nodeward runs the same steps as before, with each
// request value passed as one argument or as one quoted shell word.
func TestUpgradeNodeK8sRunsTheExpectedCommandsForAValidRequest(t *testing.T) {
	rec := &upgradeRecorder{target: "1.35.9"}
	rec.install(t)
	req := validUpgradeRequest()
	req.TargetContainerdVersion = "2.2.2"
	req.TargetHelmVersion = "3.22.1"

	resp, err := handleUpgradeNodeK8s(upgradeInstruction(t, req), &ProgressTagger{})
	if err != nil {
		t.Fatal(err)
	}
	if got := upgradeResult(t, resp); !got.Success || got.NewVersion != "1.35.9" || got.PreviousVersion != "1.35.4" {
		t.Fatalf("result = %+v, want success 1.35.4 -> 1.35.9", got)
	}
	want := []string{
		"sh: kubelet --version",
		"argv: curl [-sS] [-o] [/dev/null] [https://pkgs.k8s.io]",
		"argv: mkdir [-p] [/etc/apt/keyrings]",
		"argv: curl [-fsSL] [https://pkgs.k8s.io/core:/stable:/v1.35/deb/Release.key] [-o] [/tmp/kubernetes-release.key]",
		"argv: gpg [--batch] [--yes] [--dearmor] [-o] [/etc/apt/keyrings/kubernetes-apt-keyring.gpg] [/tmp/kubernetes-release.key]",
		"write: /etc/apt/sources.list.d/kubernetes.list",
		"sh: apt-get update",
		"sh: " + commons.AptMarkInstalledCommand("unhold", commons.UpgradeMovedPackages),
		"sh: apt-mark unhold kubeadm",
		"sh: apt-get install -y 'kubeadm=1.35.9-*'",
		"sh: apt-mark hold kubeadm",
		"argv: kubeadm [upgrade] [apply] [v1.35.9] [--yes]",
		"sh: apt-mark unhold kubelet kubectl",
		"sh: apt-get install -y 'kubelet=1.35.9-*' 'kubectl=1.35.9-*'",
		"sh: apt-mark hold kubelet kubectl",
		"sh: dpkg-query -W -f='${Version}' containerd",
		"sh: apt-get install -y 'containerd=2.2.2-*'",
		"sh: " + commons.AptMarkInstalledCommand("hold", commons.UpgradeMovedPackages),
		"sh: helm version --short --client",
		"sh: curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | DESIRED_VERSION='v3.22.1' bash",
		"sh: systemctl daemon-reload && systemctl restart kubelet",
		"argv: kubectl [get] [node] [node-a] [-o] [jsonpath={.status.nodeInfo.kubeletVersion}]",
		`argv: kubectl [get] [node] [node-a] [-o] [jsonpath={.status.conditions[?(@.type=="Ready")].status}]`,
	}
	if strings.Join(rec.commands, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(rec.commands, "\n"), strings.Join(want, "\n"))
	}
	wantSource := "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.35/deb/ /\n"
	if got := rec.files["/etc/apt/sources.list.d/kubernetes.list"]; got != wantSource {
		t.Errorf("kubernetes.list = %q, want %q", got, wantSource)
	}
}

// A worker node runs `kubeadm upgrade node`, not `upgrade apply`.
func TestUpgradeNodeK8sOnAWorkerRunsUpgradeNode(t *testing.T) {
	rec := &upgradeRecorder{target: "1.35.9"}
	rec.install(t)
	req := validUpgradeRequest()
	req.IsFirstCp = false
	resp, _ := handleUpgradeNodeK8s(upgradeInstruction(t, req), &ProgressTagger{})
	if !upgradeResult(t, resp).Success {
		t.Fatalf("worker upgrade failed: %+v", upgradeResult(t, resp))
	}
	found := false
	for _, c := range rec.commands {
		if c == "argv: kubeadm [upgrade] [node]" {
			found = true
		}
		if strings.Contains(c, "upgrade] [apply") {
			t.Errorf("a worker must not run kubeadm upgrade apply")
		}
	}
	if !found {
		t.Errorf("a worker must run kubeadm upgrade node; got %q", rec.commands)
	}
}

// The values nodeward ships today pass the check (config/node_dependency_versions.go).
func TestUpgradeValidationAcceptsTheNodewardMatrixValues(t *testing.T) {
	for _, req := range []upgradeNodeK8sRequest{
		{TargetKubernetesVersion: "1.33.4", TargetContainerdVersion: "2.2.1", TargetHelmVersion: "3.22.0", KubeRepoVersion: "v1.33"},
		{TargetKubernetesVersion: "1.34.12", TargetContainerdVersion: "2.2.1", TargetHelmVersion: "3.22.0", KubeRepoVersion: "v1.34"},
		{TargetKubernetesVersion: "1.35.9", TargetContainerdVersion: "2.2.2", TargetHelmVersion: "3.22.0", KubeRepoVersion: "v1.35"},
		{TargetKubernetesVersion: "1.36.5", TargetContainerdVersion: "2.2.2", TargetHelmVersion: "3.22.0", KubeRepoVersion: "v1.36"},
	} {
		if err := validateUpgradeNodeK8sRequest(req); err != nil {
			t.Errorf("%+v: %v", req, err)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"kubeadm=1.35.9-*": "'kubeadm=1.35.9-*'",
		"a'b":              `'a'\''b'`,
		"":                 "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func init() {
	// No test may wait on a real clock.
	upgradeSleep = func(time.Duration) {}
}

// FCR 933: node install holds containerd, cri-tools and kubernetes-cni, so the
// explicit Kubernetes upgrade must release them before it installs and hold
// them again afterwards.
func TestUpgradeNodeK8sReleasesAndRestoresTheRuntimeHolds(t *testing.T) {
	rec := &upgradeRecorder{target: "1.35.9"}
	rec.install(t)
	req := validUpgradeRequest()
	req.TargetContainerdVersion = "2.2.2"

	resp, _ := handleUpgradeNodeK8s(upgradeInstruction(t, req), &ProgressTagger{})
	if !upgradeResult(t, resp).Success {
		t.Fatalf("upgrade failed: %+v", upgradeResult(t, resp))
	}
	unhold := "sh: " + commons.AptMarkInstalledCommand("unhold", commons.UpgradeMovedPackages)
	hold := "sh: " + commons.AptMarkInstalledCommand("hold", commons.UpgradeMovedPackages)
	idx := func(want string) []int {
		var at []int
		for i, c := range rec.commands {
			if c == want {
				at = append(at, i)
			}
		}
		return at
	}
	firstInstall := -1
	containerdInstall := -1
	for i, c := range rec.commands {
		if strings.HasPrefix(c, "sh: apt-get install") && firstInstall < 0 {
			firstInstall = i
		}
		if strings.HasPrefix(c, "sh: apt-get install -y 'containerd=") {
			containerdInstall = i
		}
	}
	u, h := idx(unhold), idx(hold)
	if len(u) != 1 || len(h) != 1 {
		t.Fatalf("want one unhold and one hold, got %d and %d in %q", len(u), len(h), rec.commands)
	}
	if !(u[0] < firstInstall) {
		t.Errorf("the hold must be released before the first apt-get install (unhold %d, install %d)", u[0], firstInstall)
	}
	if !(h[0] > containerdInstall) {
		t.Errorf("the hold must come back after the containerd install (hold %d, install %d)", h[0], containerdInstall)
	}
}

// A containerd that is already at the target still gets its hold back.
func TestUpgradeNodeK8sHoldsAgainWhenContainerdIsAlreadyAtTarget(t *testing.T) {
	rec := &upgradeRecorder{target: "1.35.9"}
	rec.install(t)
	req := validUpgradeRequest() // containerd 2.2.1 equals the recorder's installed 2.2.1
	resp, _ := handleUpgradeNodeK8s(upgradeInstruction(t, req), &ProgressTagger{})
	if !upgradeResult(t, resp).Success {
		t.Fatalf("upgrade failed: %+v", upgradeResult(t, resp))
	}
	hold := "sh: " + commons.AptMarkInstalledCommand("hold", commons.UpgradeMovedPackages)
	for _, c := range rec.commands {
		if strings.HasPrefix(c, "sh: apt-get install -y 'containerd=") {
			t.Fatalf("containerd must not be installed when already at target: %q", rec.commands)
		}
	}
	found := false
	for _, c := range rec.commands {
		found = found || c == hold
	}
	if !found {
		t.Errorf("hold missing in %q", rec.commands)
	}
}

// A failure in the middle of the upgrade must not leave the runtime packages
// open: the hold comes back on the way out.
func TestUpgradeNodeK8sRestoresTheHoldWhenTheUpgradeFails(t *testing.T) {
	rec := &upgradeRecorder{target: "1.35.9"}
	rec.install(t)
	record := upgradeRunShell
	upgradeRunShell = func(c string) (string, error) {
		if strings.Contains(c, "apt-get install -y 'kubeadm=") {
			rec.commands = append(rec.commands, "sh: "+c)
			return "E: unable to locate package", errFakeApt
		}
		return record(c)
	}
	resp, _ := handleUpgradeNodeK8s(upgradeInstruction(t, validUpgradeRequest()), &ProgressTagger{})
	if upgradeResult(t, resp).Success {
		t.Fatal("expected the upgrade to fail")
	}
	hold := "sh: " + commons.AptMarkInstalledCommand("hold", commons.UpgradeMovedPackages)
	if last := rec.commands[len(rec.commands)-1]; last != hold {
		t.Fatalf("the last command must put the hold back, got %q", last)
	}
}

// If the hold cannot be released the upgrade stops before it installs anything.
func TestUpgradeNodeK8sStopsWhenTheHoldCannotBeReleased(t *testing.T) {
	rec := &upgradeRecorder{target: "1.35.9"}
	rec.install(t)
	record := upgradeRunShell
	upgradeRunShell = func(c string) (string, error) {
		if strings.Contains(c, "apt-mark unhold \"$p\"") {
			rec.commands = append(rec.commands, "sh: "+c)
			return "E: boom", errFakeApt
		}
		return record(c)
	}
	resp, _ := handleUpgradeNodeK8s(upgradeInstruction(t, validUpgradeRequest()), &ProgressTagger{})
	if upgradeResult(t, resp).Success {
		t.Fatal("expected the upgrade to fail")
	}
	for _, c := range rec.commands {
		if strings.HasPrefix(c, "sh: apt-get install") {
			t.Fatalf("nothing may be installed after a failed unhold: %q", rec.commands)
		}
	}
}

type fakeAptError struct{}

func (fakeAptError) Error() string { return "exit status 100" }

var errFakeApt error = fakeAptError{}
