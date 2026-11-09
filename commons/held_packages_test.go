package commons

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAptTools puts a fake dpkg-query and apt-mark first on PATH. dpkg-query
// reports "installed" for the names in installed and fails for the rest;
// apt-mark appends its arguments to a log and fails for any name in failFor.
// It returns the PATH to use and the log file.
func fakeAptTools(t *testing.T, installed, failFor []string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "apt-mark.log")
	dpkg := "#!/bin/sh\n" +
		"for a in \"$@\"; do last=$a; done\n" +
		"case \" " + strings.Join(installed, " ") + " \" in *\" $last \"*) printf installed; exit 0;; esac\n" +
		"exit 1\n"
	aptMark := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logFile + "\n" +
		"case \" " + strings.Join(failFor, " ") + " \" in *\" $2 \"*) exit 100;; esac\n" +
		"exit 0\n"
	for name, body := range map[string]string{"dpkg-query": dpkg, "apt-mark": aptMark} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir + ":" + os.Getenv("PATH"), logFile
}

func runFakeShell(t *testing.T, path, command string) error {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+path)
	return cmd.Run()
}

func readLog(t *testing.T, logFile string) string {
	t.Helper()
	b, err := os.ReadFile(logFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestAptMarkInstalledCommandMarksOnlyInstalledPackages(t *testing.T) {
	path, logFile := fakeAptTools(t, []string{"containerd", "kubernetes-cni"}, nil)
	if err := runFakeShell(t, path, AptMarkInstalledCommand("hold", UpgradeMovedPackages)); err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	got := readLog(t, logFile)
	if got != "hold containerd\nhold kubernetes-cni\n" {
		t.Fatalf("apt-mark calls = %q", got)
	}
}

func TestAptMarkInstalledCommandUnhold(t *testing.T) {
	path, logFile := fakeAptTools(t, UpgradeMovedPackages, nil)
	if err := runFakeShell(t, path, AptMarkInstalledCommand("unhold", UpgradeMovedPackages)); err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if got := readLog(t, logFile); got != "unhold containerd\nunhold cri-tools\nunhold kubernetes-cni\n" {
		t.Fatalf("apt-mark calls = %q", got)
	}
}

func TestAptMarkInstalledCommandFailsWhenAptMarkFails(t *testing.T) {
	path, _ := fakeAptTools(t, UpgradeMovedPackages, []string{"cri-tools"})
	if err := runFakeShell(t, path, AptMarkInstalledCommand("hold", UpgradeMovedPackages)); err == nil {
		t.Fatal("expected a non-zero exit when apt-mark fails for an installed package")
	}
}

func TestAptMarkInstalledCommandWithNothingInstalledSucceeds(t *testing.T) {
	path, logFile := fakeAptTools(t, nil, nil)
	if err := runFakeShell(t, path, AptMarkInstalledCommand("hold", UpgradeMovedPackages)); err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if got := readLog(t, logFile); got != "" {
		t.Fatalf("nothing is installed, apt-mark must not run: %q", got)
	}
}

// FCR 933: uninstall purges with apt-get, which refuses held packages, so it
// must release every package the node install holds, and one failure must not
// stop the others.
func TestUnholdRunosPackagesCommandReleasesEverythingEvenWhenOneFails(t *testing.T) {
	path, logFile := fakeAptTools(t, nil, []string{"haproxy"})
	if err := runFakeShell(t, path, unholdRunosPackagesCommand()); err != nil {
		t.Fatalf("the unhold step is best effort and must exit 0, got %v", err)
	}
	got := strings.Fields(strings.ReplaceAll(readLog(t, logFile), "unhold", ""))
	want := append(append([]string{}, KubernetesHeldPackages...), RuntimeHeldPackages...)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("unheld %v, want %v", got, want)
	}
}

func TestUpgradeHeldPackagesAreReleasedByUninstall(t *testing.T) {
	// The upgrade list must be a subset of what install holds, or the upgrade
	// would hold packages the uninstall does not release.
	all := map[string]bool{}
	for _, p := range append(append([]string{}, KubernetesHeldPackages...), RuntimeHeldPackages...) {
		all[p] = true
	}
	for _, p := range UpgradeMovedPackages {
		if !all[p] {
			t.Errorf("%s is held by the upgrade but not released by the uninstall", p)
		}
	}
}

// The purge set must be a SUPERSET of the held set. apt-get refuses a held
// package, so the uninstall releases every hold first; a name it releases and
// never purges is left installed on the node. cri-tools was exactly that: the
// unhold loop released it and the purge list did not name it, so a node delete
// left cri-tools behind, with its hold released (FCR 933).
func TestPurgeCommandNamesEveryHeldPackage(t *testing.T) {
	cmd := PurgeRunosPackagesCommand()
	// The command feeds its name list to dpkg-query, so a name is "in the purge
	// set" only when it appears as a whole word in the generated command.
	for _, p := range HeldPackages {
		if !hasWord(cmd, p) {
			t.Errorf("%s is released by the unhold loop but never purged, so a node delete leaves it installed", p)
		}
	}
}

// The declaration is the only source. These relations are what "only source"
// means, and each one was broken by a hand-maintained list before (FCR 933).
func TestRunosPackagesDeclarationIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range RunosPackages {
		if p.Name == "" {
			t.Errorf("a declared package has no name")
		}
		if seen[p.Name] {
			t.Errorf("%s is declared twice, so the derived lists disagree about it", p.Name)
		}
		seen[p.Name] = true
		if p.Kubernetes && p.UpgradeMoved {
			t.Errorf("%s is both Kubernetes-only and upgrade-moved, which contradicts itself", p.Name)
		}
		if p.UpgradeMoved && !p.Held {
			t.Errorf("%s is moved by the Kubernetes upgrade but not held, so the upgrade would set a hold the uninstall never releases", p.Name)
		}
		if p.Kubernetes && !p.Held {
			t.Errorf("%s moves only through a Kubernetes upgrade but is not held, so a plain apt upgrade could move it", p.Name)
		}
	}

	// Every derived name comes from the table, so no list can carry a name the
	// declaration does not.
	for name, list := range map[string][]string{
		"KubernetesHeldPackages": KubernetesHeldPackages,
		"UpgradeMovedPackages":   UpgradeMovedPackages,
		"RuntimeHeldPackages":    RuntimeHeldPackages,
		"HeldPackages":           HeldPackages,
		"PurgePackages":          PurgePackages,
	} {
		for _, p := range list {
			if !seen[p] {
				t.Errorf("%s carries %s, which the declaration does not declare", name, p)
			}
		}
	}

	// PurgePackages is every row, so it is a superset of every other list.
	purge := map[string]bool{}
	for _, p := range PurgePackages {
		purge[p] = true
	}
	for _, p := range RunosPackages {
		if !purge[p.Name] {
			t.Errorf("%s is declared but never purged, so a node delete leaves it installed", p.Name)
		}
	}
}

// The set itself, pinned. A package added here starts being held on every node
// and purged on every node delete, so the change must be deliberate and must be
// read by a human. The Nodeward install command builder has 28 unique apt
// package specifications. Eight specifications are rows below, and 20 are not.
// Kubernetes installs kubernetes-cni as the ninth row. Wireguard is absent.
func TestRunosPackagesSetIsPinned(t *testing.T) {
	want := []string{
		"kubelet", "kubeadm", "kubectl",
		"containerd", "cri-tools", "kubernetes-cni",
		"haproxy", "dnsmasq", "wireguard-tools",
		"wireguard",
	}
	got := make([]string, 0, len(RunosPackages))
	for _, p := range RunosPackages {
		got = append(got, p.Name)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the RunOS package set is now %v, was %v; if that is deliberate, update this test and say why in the changelog", got, want)
	}

	// wireguard is the one purge-only row: the install puts wireguard-tools on a
	// node, not wireguard, so RunOS never holds it.
	for _, p := range RunosPackages {
		if p.Name == "wireguard" && p.Held {
			t.Errorf("wireguard is declared held, but the install does not install it, so the hold would be a stale selection")
		}
	}
}

// Story 295 criterion 4, named rather than only implied by the set: a node
// delete must release every hold and leave no held package and no container
// runtime command-line tool behind. cri-tools is that tool, and it is the one
// the old hand-typed purge list omitted.
func TestNodeDeleteReleasesEveryHoldAndPurgesCriTools(t *testing.T) {
	unhold := unholdRunosPackagesCommand()
	purge := PurgeRunosPackagesCommand()

	for _, p := range HeldPackages {
		if !hasWord(unhold, p) {
			t.Errorf("a node delete does not release the hold on %s, so the package purge is refused for it", p)
		}
		if !hasWord(purge, p) {
			t.Errorf("a node delete does not purge %s, so it is left installed with its hold released", p)
		}
	}

	// By name, because this is the package the defect left behind.
	if !hasWord(unhold, "cri-tools") {
		t.Error("the unhold loop does not name cri-tools")
	}
	if !hasWord(purge, "cri-tools") {
		t.Error("the purge command does not name cri-tools, which is the defect this story fixes")
	}

	// The unhold step must stay best effort: one unknown or already unheld name
	// cannot be allowed to stop the rest, or the purge is refused for the names
	// after it.
	if !strings.Contains(unhold, "|| true") {
		t.Error("the unhold loop is no longer best effort, so one failing name stops the rest")
	}
}

// hasWord reports whether a generated shell command names this package as a
// whole word. It trims the shell punctuation that attaches to a word, because
// the last name of a `for p in ... ; do` list arrives as "wireguard-tools;" and
// a bare equality test would miss it. A trimming comparison is used rather than
// a substring or word-boundary search because "wireguard" is a substring of
// "wireguard-tools" and the two are different packages here.
func hasWord(command, word string) bool {
	for _, w := range strings.Fields(command) {
		if strings.Trim(w, `;"'$()`) == word {
			return true
		}
	}
	return false
}
