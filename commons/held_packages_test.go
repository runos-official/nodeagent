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
