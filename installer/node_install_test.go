// Package installer holds the static node installer (node-install.sh) and its tests.
//
// The installer runs as root on the node, so every argument is checked with shell builtins
// BEFORE the script runs any command. These tests run the real script under bash with a PATH
// that holds only recording stubs, and assert that a hostile or malformed argument stops the
// script before ANY command runs.
package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// goodToken has the minted shape (64 letters and digits). It is built at run time so that no
// credential-shaped literal sits in the source.
var goodToken = strings.Repeat("Ab3d", 16)

const (
	goodAccount = "ab1cd"
	goodAPI     = "https://api.example.test"
)

// stubbed is every external command the script could run. Each stub records its own name.
var stubbed = []string{
	"id", "uname", "touch", "chmod", "tee", "curl", "mktemp", "sed", "head", "tr", "grep", "awk",
	"sha256sum", "install", "rm", "mkdir", "systemctl", "sudo", "sleep", "cat", "runos", "bash", "sh",
}

// runInstaller runs the script with args under bash and a stub-only PATH. It returns the exit
// code, stderr and the names of the commands the script ran, in order.
func runInstaller(t *testing.T, uid string, args ...string) (int, string, []string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	dir := t.TempDir()
	stubs := filepath.Join(dir, "bin")
	if err := os.Mkdir(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(dir, "ran.log")
	for _, name := range stubbed {
		body := "#!" + bash + "\nprintf '%s\\n' " + name + " >> \"$RAN_LOG\"\n"
		if name == "id" {
			body += "printf '%s\\n' \"$STUB_UID\"\n"
		}
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bash, append([]string{"node-install.sh"}, args...)...)
	cmd.Env = []string{"PATH=" + stubs, "RAN_LOG=" + ran, "STUB_UID=" + uid, "HOME=" + dir}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err = cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	var names []string
	if raw, readErr := os.ReadFile(ran); readErr == nil {
		names = strings.Fields(string(raw))
	}
	return code, stderr.String(), names
}

func validArgs() map[string]string {
	return map[string]string{"--api": goodAPI, "--account": goodAccount, "--token": goodToken}
}

func argList(m map[string]string) []string {
	var out []string
	for _, k := range []string{"--api", "--account", "--token", "--version"} {
		if v, ok := m[k]; ok {
			out = append(out, k, v)
		}
	}
	return out
}

// TestHostileArgumentsRunNothing is the security contract: a malformed or hostile value in any
// argument stops the script with exit 2 before ANY external command runs.
func TestHostileArgumentsRunNothing(t *testing.T) {
	hostile := []string{
		"$(touch /tmp/pwned)",
		"`touch /tmp/pwned`",
		"x; touch /tmp/pwned",
		"x\ntouch /tmp/pwned",
		"x' || touch /tmp/pwned #",
		"x\" && touch /tmp/pwned #",
		"../../etc/passwd",
		"",
		strings.Repeat("a", 4096),
		"\x1b[2J",
		"ａｂ１ｃｄ",
	}
	for _, flag := range []string{"--api", "--account", "--token", "--version"} {
		for _, value := range hostile {
			if flag == "--version" && value == "" {
				continue // an empty version means "not given", which is valid
			}
			args := validArgs()
			args[flag] = value
			code, stderr, ran := runInstaller(t, "0", argList(args)...)
			if code != 2 {
				t.Errorf("%s=%q: exit %d, want 2 (stderr %q)", flag, value, code, stderr)
			}
			if len(ran) != 0 {
				t.Errorf("%s=%q: the script ran %v before refusing", flag, value, ran)
			}
			if value != "" && strings.Contains(stderr, value) {
				t.Errorf("%s=%q: the refusal echoed the value", flag, value)
			}
		}
	}
}

// TestNearMissShapesAreRefused covers values that are one character away from a valid shape.
func TestNearMissShapesAreRefused(t *testing.T) {
	cases := map[string][]string{
		"--token":   {goodToken[:63], goodToken + "a", goodToken[:63] + "-"},
		"--account": {"ab1c", "ab1cde", "AB1CD", "ab-cd"},
		"--api":     {"http://api.example.test", "https://api.example.test/", "https://api.example.test/x", "https://-api.example.test", "https://API.example.test", "https://api.example.test:123456"},
		"--version": {"1.2", "v1.2.3.4", "1.2.3-", "1.2.3-rc 1", "latest"},
	}
	for flag, values := range cases {
		for _, value := range values {
			args := validArgs()
			args[flag] = value
			code, _, ran := runInstaller(t, "0", argList(args)...)
			if code != 2 || len(ran) != 0 {
				t.Errorf("%s=%q: exit %d and ran %v, want exit 2 and nothing run", flag, value, code, ran)
			}
		}
	}
}

func TestMissingAndUnknownArgumentsRunNothing(t *testing.T) {
	cases := [][]string{
		{},
		{"--api", goodAPI, "--account", goodAccount},
		{"--api", goodAPI, "--token", goodToken},
		{"--account", goodAccount, "--token", goodToken},
		append(argList(validArgs()), "--extra", "x"),
		append(argList(validArgs()), "--token"),
		append(argList(validArgs()), "$(touch /tmp/pwned)"),
	}
	for _, args := range cases {
		code, _, ran := runInstaller(t, "0", args...)
		if code != 2 || len(ran) != 0 {
			t.Errorf("args %q: exit %d and ran %v, want exit 2 and nothing run", args, code, ran)
		}
	}
}

// TestValidArgumentsReachTheRootCheck is the positive control: the stubs work, and valid
// arguments (both spellings) pass the checks. The first command the script runs is `id`.
func TestValidArgumentsReachTheRootCheck(t *testing.T) {
	forms := [][]string{
		argList(validArgs()),
		{"--api=" + goodAPI, "--account=" + goodAccount, "--token=" + goodToken, "--version=v1.2.3-rc.1"},
	}
	for _, args := range forms {
		code, stderr, ran := runInstaller(t, "1000", args...)
		if code != 2 || !strings.Contains(stderr, "as root") {
			t.Errorf("args %q: exit %d stderr %q, want the root refusal", args, code, stderr)
		}
		if strings.Join(ran, " ") != "id" {
			t.Errorf("args %q: ran %v, want only id", args, ran)
		}
	}
}

// TestTheScriptIsStatic guards the design: the file is served unchanged, so it must carry no
// template action for a server to fill in.
func TestTheScriptIsStatic(t *testing.T) {
	raw, err := os.ReadFile("node-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"{{", "}}"} {
		if strings.Contains(string(raw), marker) {
			t.Errorf("node-install.sh contains %q: the installer must be static", marker)
		}
	}
	if !strings.Contains(string(raw), "client.server.conductor") {
		t.Error("node-install.sh must write client.server.conductor explicitly")
	}
}
