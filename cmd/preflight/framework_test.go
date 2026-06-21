package preflight

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Pins the preflight check REGISTRY is well-formed: every entry has a non-nil fn
// and a non-empty, UNIQUE name, the fatal prerequisites are exactly the intended
// set and come first in declared order, and no check is both fatal and net. A nil
// fn or a duplicate name would panic / silently shadow a check at runtime on a
// real node during install; this catches it at test time.
func TestPreflightChecksRegistryWellFormed(t *testing.T) {
	checks := preflightChecks()
	if len(checks) == 0 {
		t.Fatal("preflightChecks() returned no checks")
	}

	seen := map[string]bool{}
	for i, c := range checks {
		if c.fn == nil {
			t.Errorf("check #%d %q has a nil fn", i, c.name)
		}
		if strings.TrimSpace(c.name) == "" {
			t.Errorf("check #%d has an empty name", i)
		}
		if seen[c.name] {
			t.Errorf("duplicate check name %q", c.name)
		}
		seen[c.name] = true

		// A fatal prerequisite is a cheap local probe; it must never be a network
		// check (network runs in a later phase that fatal-fail-fast skips).
		if c.fatal && c.net {
			t.Errorf("check %q is both fatal and net; fatal prereqs run before the network phase", c.name)
		}
	}

	// The fatal prerequisites must be EXACTLY this set, and they must come first
	// in declared order (runChecks fail-fasts them in order; a non-fatal slipping
	// in front, or a missing/extra fatal, changes the stop-early semantics).
	wantFatal := []string{
		"root", "systemd-init", "proc-sys-mounted", "cpu-arch",
		"os-version", "virtualization", "base-tooling",
	}
	var gotFatal []string
	sawNonFatal := false
	for _, c := range checks {
		if c.fatal {
			if sawNonFatal {
				t.Errorf("fatal check %q appears after a non-fatal check; fatals must lead", c.name)
			}
			gotFatal = append(gotFatal, c.name)
		} else {
			sawNonFatal = true
		}
	}
	if strings.Join(gotFatal, ",") != strings.Join(wantFatal, ",") {
		t.Errorf("fatal prerequisites = %v, want %v (exact set + order)", gotFatal, wantFatal)
	}
}

// Pins the phase-runner contract install depends on: blocks AND warns are all
// collected in one pass (not fail-fast), a failing fatal prereq STOPS the run
// before any later check executes, warns never fail the run, and blocks do. A
// regression here would either hide problems (fail-fast) or block installs on
// advisory warnings.
func TestRunChecksCollectsAndRoutes(t *testing.T) {
	// reportFatal/report write to os.Stderr; silence it for the whole test.
	restore := silenceStderr(t)
	defer restore()

	pass := func() error { return nil }
	fail := func(name string) func() error {
		return func() error { return fmt.Errorf("%s failed", name) }
	}

	t.Run("collects all blocks and warns in one pass (no fail-fast)", func(t *testing.T) {
		var ran []string
		track := func(name string, fn func() error) func() error {
			return func() error { ran = append(ran, name); return fn() }
		}
		checks := []check{
			{name: "b1", fn: track("b1", fail("b1")), sev: sevBlock},
			{name: "w1", fn: track("w1", fail("w1")), sev: sevWarn},
			{name: "b2", fn: track("b2", fail("b2")), sev: sevBlock},
			{name: "ok", fn: track("ok", pass), sev: sevBlock},
		}
		err := runChecks(checks)
		if err == nil {
			t.Fatal("want error because there are blocking findings, got nil")
		}
		// Every check ran despite the first one failing (no fail-fast in this phase).
		if len(ran) != 4 {
			t.Errorf("expected all 4 checks to run, ran %v", ran)
		}
		// The error names the blocking-count (2), not the warning.
		if !strings.Contains(err.Error(), "2") {
			t.Errorf("error %q should report 2 blocking failures", err.Error())
		}
	})

	t.Run("warnings alone never fail the run", func(t *testing.T) {
		checks := []check{
			{name: "w1", fn: fail("w1"), sev: sevWarn},
			{name: "w2", fn: fail("w2"), sev: sevWarn},
			{name: "ok", fn: pass, sev: sevBlock},
		}
		if err := runChecks(checks); err != nil {
			t.Fatalf("warnings must not fail the run, got: %v", err)
		}
	})

	t.Run("a single block fails the run", func(t *testing.T) {
		checks := []check{{name: "b1", fn: fail("b1"), sev: sevBlock}}
		if err := runChecks(checks); err == nil {
			t.Fatal("a blocking finding must fail the run")
		}
	})

	t.Run("fatal prereq stops before later checks run", func(t *testing.T) {
		var ran []string
		track := func(name string, fn func() error) func() error {
			return func() error { ran = append(ran, name); return fn() }
		}
		checks := []check{
			{name: "fatal", fn: track("fatal", fail("fatal")), sev: sevBlock, fatal: true},
			{name: "later", fn: track("later", pass), sev: sevBlock},
			{name: "later-net", fn: track("later-net", pass), sev: sevBlock, net: true},
		}
		err := runChecks(checks)
		if err == nil {
			t.Fatal("a failing fatal prereq must fail the run")
		}
		if !strings.Contains(err.Error(), "fatal") {
			t.Errorf("error %q should name the fatal check", err.Error())
		}
		// The whole point of "fatal": nothing after it runs.
		if len(ran) != 1 || ran[0] != "fatal" {
			t.Errorf("expected only the fatal check to run, ran %v", ran)
		}
	})

	t.Run("all-pass run succeeds", func(t *testing.T) {
		checks := []check{
			{name: "f", fn: pass, sev: sevBlock, fatal: true},
			{name: "l", fn: pass, sev: sevBlock},
			{name: "n", fn: pass, sev: sevBlock, net: true},
		}
		if err := runChecks(checks); err != nil {
			t.Fatalf("all-pass run should succeed, got: %v", err)
		}
	})
}

// Pins indentLines: 2nd..nth lines of a multi-line remedy are indented so they
// line up under the check header in the report (a flush-left remedy is a visible
// formatting regression in the operator-facing output).
func TestIndentLines(t *testing.T) {
	if got := indentLines("single"); got != "single" {
		t.Errorf("single line should be unchanged, got %q", got)
	}
	got := indentLines("line1\nline2\nline3")
	want := "line1\n  line2\n  line3"
	if got != want {
		t.Errorf("indentLines = %q, want %q", got, want)
	}
}

// silenceStderr redirects os.Stderr to /dev/null for the duration of a test so
// the report writers don't spam the test log, restoring it on cleanup.
func silenceStderr(t *testing.T) func() {
	t.Helper()
	orig := os.Stderr
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	os.Stderr = devnull
	return func() {
		os.Stderr = orig
		devnull.Close()
	}
}
