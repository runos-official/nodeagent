package agentstream

import (
	"os"
	"strings"
)

type savedUpgradeRunners struct {
	shell      func(string) (string, error)
	argv       func(string, ...string) (string, error)
	streamArgv func(func(string), string, ...string) (string, error)
	writeFile  func(string, []byte, os.FileMode) error
	hostname   func() (string, error)
}

func saveUpgradeRunners() savedUpgradeRunners {
	return savedUpgradeRunners{upgradeRunShell, upgradeRunArgv, upgradeStreamArgv, upgradeWriteFile, upgradeHostname}
}

func restoreUpgradeRunners(s savedUpgradeRunners) {
	upgradeRunShell, upgradeRunArgv, upgradeStreamArgv, upgradeWriteFile, upgradeHostname =
		s.shell, s.argv, s.streamArgv, s.writeFile, s.hostname
}

// installUpgradeRecorder records shell commands as "sh: <command>" and direct
// commands as "argv: <name> <arg> ...", with each argument in brackets.
func installUpgradeRecorder(r *upgradeRecorder) {
	argv := func(name string, args ...string) string {
		parts := []string{"argv:", name}
		for _, a := range args {
			parts = append(parts, "["+a+"]")
		}
		return strings.Join(parts, " ")
	}
	upgradeRunShell = func(c string) (string, error) { return r.record("sh: " + c) }
	upgradeRunArgv = func(name string, args ...string) (string, error) { return r.record(argv(name, args...)) }
	upgradeStreamArgv = func(_ func(string), name string, args ...string) (string, error) {
		return r.record(argv(name, args...))
	}
	upgradeWriteFile = func(path string, data []byte, _ os.FileMode) error {
		r.files[path] = string(data)
		_, err := r.record("write: " + path)
		return err
	}
	upgradeHostname = func() (string, error) { return "node-a", nil }
}
