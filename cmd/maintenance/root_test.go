package maintenance

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type fakePackageRunner struct {
	installed   map[string]string
	candidate   map[string]string
	held        map[string]bool
	fail        map[string]error
	skipInstall map[string]bool
	calls       []string
}

func newFakePackageRunner() *fakePackageRunner {
	return &fakePackageRunner{
		installed:   map[string]string{},
		candidate:   map[string]string{},
		held:        map[string]bool{},
		fail:        map[string]error{},
		skipInstall: map[string]bool{},
	}
}

func (f *fakePackageRunner) installedVersion(name string) (string, bool, error) {
	f.calls = append(f.calls, "installed:"+name)
	if err := f.fail["installed:"+name]; err != nil {
		return "", false, err
	}
	v, ok := f.installed[name]
	return v, ok, nil
}

func (f *fakePackageRunner) candidateVersion(name string) (string, bool, error) {
	f.calls = append(f.calls, "candidate:"+name)
	if err := f.fail["candidate:"+name]; err != nil {
		return "", false, err
	}
	v, ok := f.candidate[name]
	return v, ok, nil
}

func (f *fakePackageRunner) setHold(name string, hold bool) error {
	action := "unhold:"
	if hold {
		action = "hold:"
	}
	f.calls = append(f.calls, action+name)
	if err := f.fail[action+name]; err != nil {
		return err
	}
	f.held[name] = hold
	return nil
}

func (f *fakePackageRunner) installVersion(name, version string) error {
	f.calls = append(f.calls, "install:"+name+"="+version)
	if err := f.fail["install:"+name]; err != nil {
		return err
	}
	if !f.skipInstall[name] {
		f.installed[name] = version
	}
	return nil
}

func (f *fakePackageRunner) isHeld(name string) (bool, error) {
	f.calls = append(f.calls, "held:"+name)
	if err := f.fail["held:"+name]; err != nil {
		return false, err
	}
	return f.held[name], nil
}

func TestRunPackagesOrdersUnholdInstallHoldAndReadback(t *testing.T) {
	runner := newFakePackageRunner()
	runner.installed["containerd"] = "1.7.24-1ubuntu1"
	runner.candidate["containerd"] = "1.7.24-1ubuntu2"
	runner.held["containerd"] = true

	report, err := runPackages(runner, []string{"containerd"}, false)
	if err != nil {
		t.Fatalf("runPackages() error = %v", err)
	}
	wantCalls := []string{
		"installed:containerd",
		"candidate:containerd",
		"unhold:containerd",
		"install:containerd=1.7.24-1ubuntu2",
		"installed:containerd",
		"hold:containerd",
		"held:containerd",
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, wantCalls)
	}
	if report.Status != statusCompleted {
		t.Errorf("status = %q, want %q", report.Status, statusCompleted)
	}
	if len(report.Packages) != 1 || !report.Packages[0].Moved || !report.Packages[0].HeldAfter {
		t.Fatalf("package result = %#v, want moved and held", report.Packages)
	}
}

func TestRunPackagesDoesNotClaimMoveWithoutVersionReadback(t *testing.T) {
	runner := newFakePackageRunner()
	runner.installed["containerd"] = "1.7.24-1ubuntu1"
	runner.candidate["containerd"] = "1.7.24-1ubuntu2"
	runner.held["containerd"] = true
	runner.skipInstall["containerd"] = true

	report, err := runPackages(runner, []string{"containerd"}, false)
	if err != nil {
		t.Fatalf("runPackages() error = %v, want a verified final hold to keep the command safe", err)
	}
	got := report.Packages[0]
	if got.Moved || got.Reason != reasonInstallUnverified {
		t.Fatalf("result = %#v, want an unverified install outcome", got)
	}
	if !got.HeldAfter {
		t.Fatal("package is not held after the unverified install")
	}
}

func TestRunPackagesReholdsAfterInstallFailure(t *testing.T) {
	runner := newFakePackageRunner()
	runner.installed["containerd"] = "1.7.24-1ubuntu1"
	runner.candidate["containerd"] = "1.7.24-1ubuntu2"
	runner.held["containerd"] = true
	runner.fail["install:containerd"] = errors.New("install failed")

	report, err := runPackages(runner, []string{"containerd"}, false)
	if err != nil {
		t.Fatalf("runPackages() error = %v, want a safe completed command", err)
	}
	wantTail := []string{"unhold:containerd", "install:containerd=1.7.24-1ubuntu2", "hold:containerd", "held:containerd"}
	if !reflect.DeepEqual(runner.calls[len(runner.calls)-len(wantTail):], wantTail) {
		t.Fatalf("call tail = %#v, want %#v", runner.calls, wantTail)
	}
	if got := report.Packages[0].Reason; got != reasonInstallFailed {
		t.Errorf("reason = %q, want %q", got, reasonInstallFailed)
	}
	if !report.Packages[0].HeldAfter {
		t.Error("package is not held after the failed install")
	}
}

func TestRunPackagesReholdsAfterUnholdFailure(t *testing.T) {
	runner := newFakePackageRunner()
	runner.installed["containerd"] = "1.7.24-1ubuntu1"
	runner.candidate["containerd"] = "1.7.24-1ubuntu2"
	runner.held["containerd"] = true
	runner.fail["unhold:containerd"] = errors.New("unhold failed")

	report, err := runPackages(runner, []string{"containerd"}, false)
	if err != nil {
		t.Fatalf("runPackages() error = %v, want a safe completed command", err)
	}
	wantTail := []string{"unhold:containerd", "hold:containerd", "held:containerd"}
	if !reflect.DeepEqual(runner.calls[len(runner.calls)-len(wantTail):], wantTail) {
		t.Fatalf("call tail = %#v, want %#v", runner.calls, wantTail)
	}
	if got := report.Packages[0].Reason; got != reasonUnholdFailed {
		t.Errorf("reason = %q, want %q", got, reasonUnholdFailed)
	}
}

func TestRunPackagesFailsWhenFinalHoldReadbackIsFalse(t *testing.T) {
	runner := newFakePackageRunner()
	runner.installed["containerd"] = "1.7.24-1ubuntu1"
	runner.candidate["containerd"] = "1.7.24-1ubuntu2"
	runner.held["containerd"] = true
	runner.fail["hold:containerd"] = errors.New("hold failed")

	report, err := runPackages(runner, []string{"containerd"}, false)
	if err == nil {
		t.Fatal("runPackages() error = nil, want final hold failure")
	}
	if report.Status != statusFailed {
		t.Errorf("status = %q, want %q", report.Status, statusFailed)
	}
	if report.Packages[0].HeldAfter {
		t.Error("heldAfter = true, want false")
	}
}

func TestRunPackagesDryRunDoesNotMutate(t *testing.T) {
	runner := newFakePackageRunner()
	runner.installed["containerd"] = "1.7.24-1ubuntu1"
	runner.candidate["containerd"] = "1.7.24-1ubuntu2"
	runner.held["containerd"] = true

	report, err := runPackages(runner, []string{"containerd"}, true)
	if err != nil {
		t.Fatalf("runPackages() error = %v", err)
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "unhold:") || strings.HasPrefix(call, "install:") || strings.HasPrefix(call, "hold:") {
			t.Fatalf("dry-run mutation call = %q", call)
		}
	}
	if report.Packages[0].Reason != reasonDryRun {
		t.Errorf("reason = %q, want %q", report.Packages[0].Reason, reasonDryRun)
	}
}

func TestRunPackagesMovesOnlyNewerRevisionOfSameUpstream(t *testing.T) {
	cases := []struct {
		name       string
		installed  string
		candidate  string
		wantMove   bool
		wantReason string
	}{
		{name: "newer revision", installed: "2.8.5-1ubuntu3", candidate: "2.8.5-1ubuntu4", wantMove: true},
		{name: "newer upstream", installed: "2.8.5-1ubuntu3", candidate: "2.9.0-1ubuntu1", wantReason: reasonNotNewerRevision},
		{name: "older revision", installed: "2.8.5-1ubuntu4", candidate: "2.8.5-1ubuntu3", wantReason: reasonNotNewerRevision},
		{name: "unreadable candidate", installed: "2.8.5-1ubuntu3", candidate: "not-a-version", wantReason: reasonUnreadableVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakePackageRunner()
			runner.installed["containerd"] = tc.installed
			runner.candidate["containerd"] = tc.candidate
			runner.held["containerd"] = true

			report, err := runPackages(runner, []string{"containerd"}, false)
			if err != nil {
				t.Fatalf("runPackages() error = %v", err)
			}
			got := report.Packages[0]
			if got.Moved != tc.wantMove || got.Reason != tc.wantReason {
				t.Fatalf("result = %#v, want moved %v and reason %q", got, tc.wantMove, tc.wantReason)
			}
			if !tc.wantMove {
				for _, call := range runner.calls {
					if strings.HasPrefix(call, "unhold:") || strings.HasPrefix(call, "install:") || strings.HasPrefix(call, "hold:") {
						t.Fatalf("non-moving decision made mutation call %q", call)
					}
				}
			}
		})
	}
}

func TestParsePackageListAcceptsExplicitList(t *testing.T) {
	got, err := parsePackageList("containerd,runc,etcd-client")
	if err != nil {
		t.Fatalf("parsePackageList() error = %v", err)
	}
	want := []string{"containerd", "runc", "etcd-client"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("packages = %#v, want %#v", got, want)
	}
}

func TestDefaultPackageListExcludesKubernetesPackages(t *testing.T) {
	got := defaultPackageList()
	for _, protected := range []string{"kubelet", "kubeadm", "kubectl"} {
		if slices.Contains(got, protected) {
			t.Fatalf("default package list includes protected package %q", protected)
		}
	}
	for _, runtime := range []string{"containerd", "cri-tools", "kubernetes-cni", "haproxy", "dnsmasq", "wireguard-tools"} {
		if !slices.Contains(got, runtime) {
			t.Fatalf("default package list omits %q", runtime)
		}
	}
}

func TestParsePackageListRejectsKubernetesPackage(t *testing.T) {
	for _, name := range []string{"kubelet", "kubeadm", "kubectl"} {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePackageList("containerd," + name); err == nil {
				t.Fatalf("parsePackageList() accepted protected package %q", name)
			}
		})
	}
}

func TestParsePackageListRejectsInvalidInputBeforeMutation(t *testing.T) {
	for _, input := range []string{"containerd,,runc", "containerd,", "containerd;reboot", "Containerd", "containerd,containerd", "curl"} {
		t.Run(input, func(t *testing.T) {
			if _, err := parsePackageList(input); err == nil {
				t.Fatalf("parsePackageList(%q) error = nil", input)
			}
		})
	}
}

func TestParseCandidateVersionFromAptCachePolicy(t *testing.T) {
	output := "containerd:\n  Installed: 1.7.24-1ubuntu1\n  Candidate: 1.7.24-1ubuntu2\n  Version table:\n"
	got, ok, err := parseCandidateVersion(output)
	if err != nil {
		t.Fatalf("parseCandidateVersion() error = %v", err)
	}
	if !ok || got != "1.7.24-1ubuntu2" {
		t.Fatalf("candidate = %q, %v, want 1.7.24-1ubuntu2, true", got, ok)
	}

	got, ok, err = parseCandidateVersion("containerd:\n  Installed: (none)\n  Candidate: (none)\n")
	if err != nil || ok || got != "" {
		t.Fatalf("no candidate = %q, %v, %v, want empty, false, nil", got, ok, err)
	}

	if _, _, err := parseCandidateVersion("containerd:\n  Installed: 1.7.24-1ubuntu1\n"); err == nil {
		t.Fatal("missing Candidate line returned no error")
	}
}

func TestPackagesCommandEmitsOneJSONObject(t *testing.T) {
	runner := newFakePackageRunner()
	runner.installed["containerd"] = "1.7.24-1ubuntu1"
	runner.candidate["containerd"] = "1.7.24-1ubuntu2"
	runner.held["containerd"] = true

	cmd := newRootCmd(runner)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"packages", "--packages", "containerd", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v, stderr = %q", err, stderr.String())
	}
	want := `{"status":"completed","packages":[{"name":"containerd","installedVersion":"1.7.24-1ubuntu1","candidateVersion":"1.7.24-1ubuntu2","moved":true,"heldAfter":true}]}` + "\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}
