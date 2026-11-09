package patchfacts

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain pins the test process to UTC.
//
// The apt history log carries LOCAL times, which is apt's own choice, so the
// parser reads them in the node's zone. Pinning the zone here makes the
// expected times and the shared record fixture the same on every machine.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}

// fakeNode is a node made of captured command output and file contents. Every
// field is the output of one read in the command table, so a test states the
// node it wants and the collector sees nothing else.
type fakeNode struct {
	unameOut string
	unameErr error

	dpkgKernelsOut string
	dpkgKernelsErr error

	showholdOut string
	showholdErr error

	dpkgVersionsOut string

	simulateOut string
	simulateErr error

	policyIndexesOut  string
	policyPackagesOut string

	aptConfigOut string
	aptConfigErr error

	unitOut map[string]string
	unitErr error

	refreshErr   error
	refreshCalls int

	lockIsHeld bool
	lockHolder string
	lockErr    error

	files    map[string]string
	modTimes map[string]time.Time
	dirs     map[string][]fakeDirEntry

	now time.Time

	// calls records every argv the collector ran, so a test can assert what
	// did and did not reach the node.
	calls []string
}

type fakeDirEntry struct {
	name    string
	dir     bool
	modTime time.Time
}

func (e fakeDirEntry) Name() string      { return e.name }
func (e fakeDirEntry) IsDir() bool       { return e.dir }
func (e fakeDirEntry) Type() fs.FileMode { return 0 }
func (e fakeDirEntry) Info() (fs.FileInfo, error) {
	return fakeFileInfo{name: e.name, modTime: e.modTime, dir: e.dir}, nil
}

type fakeFileInfo struct {
	name    string
	modTime time.Time
	dir     bool
}

func (i fakeFileInfo) Name() string       { return i.name }
func (i fakeFileInfo) Size() int64        { return 0 }
func (i fakeFileInfo) Mode() fs.FileMode  { return 0 }
func (i fakeFileInfo) ModTime() time.Time { return i.modTime }
func (i fakeFileInfo) IsDir() bool        { return i.dir }
func (i fakeFileInfo) Sys() any           { return nil }

func notExist(path string) error {
	return &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
}

// collector wires the fake node into a Collector.
func (n *fakeNode) collector() *Collector {
	if n.now.IsZero() {
		n.now = time.Unix(1794163948, 0).UTC()
	}
	return &Collector{
		run: func(name string, args ...string) (string, error) {
			n.calls = append(n.calls, strings.Join(append([]string{name}, args...), " "))
			return n.dispatch(name, args)
		},
		readFile: func(path string) ([]byte, error) {
			body, ok := n.files[path]
			if !ok {
				return nil, notExist(path)
			}
			return []byte(body), nil
		},
		stat: func(path string) (os.FileInfo, error) {
			t, ok := n.modTimes[path]
			if !ok {
				return nil, notExist(path)
			}
			return fakeFileInfo{name: filepath.Base(path), modTime: t}, nil
		},
		readDir: func(path string) ([]os.DirEntry, error) {
			entries, ok := n.dirs[path]
			if !ok {
				return nil, notExist(path)
			}
			out := make([]os.DirEntry, 0, len(entries))
			for _, e := range entries {
				out = append(out, e)
			}
			return out, nil
		},
		lockHeld: func() (bool, string, error) { return n.lockIsHeld, n.lockHolder, n.lockErr },
		now:      func() time.Time { return n.now },
	}
}

// dispatch answers one command the way the node would.
func (n *fakeNode) dispatch(name string, args []string) (string, error) {
	joined := strings.Join(args, " ")
	switch name {
	case "uname":
		return n.unameOut, n.unameErr
	case "dpkg-query":
		if strings.Contains(joined, "linux-image-*") {
			return n.dpkgKernelsOut, n.dpkgKernelsErr
		}
		return n.dpkgVersionsOut, nil
	case "apt-mark":
		return n.showholdOut, n.showholdErr
	case "apt-get":
		if strings.Contains(joined, "update") {
			n.refreshCalls++
			return "", n.refreshErr
		}
		return n.simulateOut, n.simulateErr
	case "apt-cache":
		if len(args) > 1 {
			return n.policyPackagesOut, nil
		}
		return n.policyIndexesOut, nil
	case "apt-config":
		return n.aptConfigOut, n.aptConfigErr
	case "systemctl":
		if n.unitErr != nil {
			return "", n.unitErr
		}
		unit := args[len(args)-1]
		out, ok := n.unitOut[unit]
		if !ok {
			return "", notExist(unit)
		}
		return out, nil
	}
	return "", notExist(name)
}

func (n *fakeNode) ran(substr string) bool {
	for _, c := range n.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// maskedUnits returns the five automatic units, each with the captured masked
// output of the captured control plane node.
func maskedUnits(t *testing.T) map[string]string {
	t.Helper()
	return unitsFromBlocks(t, fixture(t, "systemctl_show_masked_units.txt"))
}

// unitsFromBlocks splits a captured systemctl show capture into one block per
// unit, keyed by the unit's Id.
func unitsFromBlocks(t *testing.T, capture string) map[string]string {
	t.Helper()
	out := map[string]string{}
	var current string
	var block []string
	flush := func() {
		if current != "" {
			out[current] = strings.Join(block, "\n") + "\n"
		}
		current, block = "", nil
	}
	for _, line := range strings.Split(capture, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if id, ok := strings.CutPrefix(line, "Id="); ok {
			flush()
			current = id
		}
		block = append(block, line)
	}
	flush()
	return out
}

// capturedNode is the node the fixtures were captured from: a RunOS control
// plane on Ubuntu 24.04.4 with the three Kubernetes packages held, the
// automatic units masked, no pending reboot, and package metadata about six
// days old. testdata/README.md records what each fixture is.
func capturedNode(t *testing.T) *fakeNode {
	t.Helper()
	return &fakeNode{
		unameOut:          "6.8.0-147-generic\n",
		dpkgKernelsOut:    fixture(t, "dpkg_query_kernels_generic.txt"),
		showholdOut:       fixture(t, "apt_mark_showhold_kube_trio.txt"),
		dpkgVersionsOut:   fixture(t, "dpkg_query_held_versions_kube_trio.txt"),
		simulateOut:       fixture(t, "apt_get_s_upgrade.txt"),
		policyIndexesOut:  fixture(t, "apt_cache_policy_indexes.txt"),
		policyPackagesOut: fixture(t, "apt_cache_policy_packages.txt"),
		aptConfigOut:      fixture(t, "apt_config_periodic_ubuntu_default.txt"),
		unitOut:           maskedUnits(t),
		files: map[string]string{
			pathProcStat:   "cpu  1 2 3\nbtime 1793700929\nprocesses 100\n",
			pathOSRelease:  "NAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\nID=ubuntu\nUBUNTU_CODENAME=noble\n",
			pathAptHistory: fixture(t, "apt_history.log"),
		},
		modTimes: map[string]time.Time{
			pathUpdateSuccessStamp: time.Unix(1793656209, 0).UTC(),
		},
		now: time.Unix(1794163948, 0).UTC(),
	}
}

// capturedSecurityNames are the twenty-five security updates waiting on that node,
// as the collector's rule names them. The set was cross-checked against the
// distribution's own counter on the node itself.
var capturedSecurityNames = []string{
	"distro-info-data", "git", "git-man", "haproxy", "libbrotli1", "liblzma5",
	"libnss-systemd", "libnss3", "libpam-systemd", "libsystemd-shared",
	"libsystemd0", "libudev1", "openssh-client", "openssh-server",
	"openssh-sftp-server", "python3-jwt", "systemd", "systemd-dev",
	"systemd-resolved", "systemd-sysv", "systemd-timesyncd", "tzdata", "udev",
	"xz-utils", "zlib1g",
}
