package agentstream

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestHandleUninstallNodeAcknowledgesOnlySuccessfulScheduling(t *testing.T) {
	t.Run("successful scheduling", func(t *testing.T) {
		called := false
		response, err := handleUninstallNode(func(delay int) error {
			called = true
			if delay != 3 {
				t.Fatalf("unexpected start delay: %d", delay)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("schedule uninstall: %v", err)
		}
		if !called || response != NoContentResponse {
			t.Fatal("successful scheduling did not return the acknowledgement template")
		}
	})

	t.Run("scheduling error", func(t *testing.T) {
		response, err := handleUninstallNode(func(int) error {
			return errors.New("scheduler unavailable")
		})
		if response != nil {
			t.Fatalf("scheduling failure returned acknowledgement: %+v", response)
		}
		if err == nil || err.Error() != "scheduler unavailable" {
			t.Fatalf("unexpected scheduling error: %v", err)
		}
	})
}

func TestScheduleDetachedUninstallUsesExistingCommandContract(t *testing.T) {
	const expectedScript = "sleep 3; /usr/local/bin/runos uninstall --yes; systemctl reboot"

	t.Run("systemd run", func(t *testing.T) {
		var command string
		var arguments []string
		runner := uninstallCommandRunner{
			lookPath: func(name string) (string, error) {
				if name != "systemd-run" {
					t.Fatalf("unexpected lookup: %q", name)
				}
				return "/usr/bin/systemd-run", nil
			},
			run: func(name string, args ...string) ([]byte, error) {
				command = name
				arguments = append([]string(nil), args...)
				return nil, nil
			},
			start: func(string, ...string) (int, error) {
				t.Fatal("systemd path used the setsid fallback")
				return 0, nil
			},
		}
		if err := scheduleDetachedUninstallWith(3, runner); err != nil {
			t.Fatalf("schedule with systemd-run: %v", err)
		}
		expectedArguments := []string{
			"--collect",
			"--description", "RunOS node uninstall and reboot",
			"/bin/sh", "-c", expectedScript,
		}
		if command != "/usr/bin/systemd-run" || !reflect.DeepEqual(arguments, expectedArguments) {
			t.Fatalf("unexpected systemd-run call: %q %q", command, arguments)
		}
	})

	t.Run("systemd run failure", func(t *testing.T) {
		runner := uninstallCommandRunner{
			lookPath: func(string) (string, error) { return "/usr/bin/systemd-run", nil },
			run: func(string, ...string) ([]byte, error) {
				return []byte("unit rejected"), errors.New("exit status 1")
			},
			start: func(string, ...string) (int, error) { return 0, nil },
		}
		err := scheduleDetachedUninstallWith(3, runner)
		if err == nil || !strings.Contains(err.Error(), "exit status 1") || !strings.Contains(err.Error(), "unit rejected") {
			t.Fatalf("unexpected systemd-run error: %v", err)
		}
	})

	t.Run("setsid fallback", func(t *testing.T) {
		var command string
		var arguments []string
		runner := uninstallCommandRunner{
			lookPath: func(string) (string, error) { return "", errors.New("not found") },
			run: func(string, ...string) ([]byte, error) {
				t.Fatal("fallback used systemd-run")
				return nil, nil
			},
			start: func(name string, args ...string) (int, error) {
				command = name
				arguments = append([]string(nil), args...)
				return 42, nil
			},
		}
		if err := scheduleDetachedUninstallWith(3, runner); err != nil {
			t.Fatalf("schedule with setsid: %v", err)
		}
		if command != "setsid" || !reflect.DeepEqual(arguments, []string{"/bin/sh", "-c", expectedScript}) {
			t.Fatalf("unexpected setsid call: %q %q", command, arguments)
		}
	})

	t.Run("setsid failure", func(t *testing.T) {
		runner := uninstallCommandRunner{
			lookPath: func(string) (string, error) { return "", errors.New("not found") },
			run:      func(string, ...string) ([]byte, error) { return nil, nil },
			start:    func(string, ...string) (int, error) { return 0, errors.New("fork failed") },
		}
		err := scheduleDetachedUninstallWith(3, runner)
		if err == nil || err.Error() != "setsid fallback failed: fork failed" {
			t.Fatalf("unexpected setsid error: %v", err)
		}
	})
}
