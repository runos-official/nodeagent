package agentstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/uc/patchfacts"
)

func TestBuildRemoteScriptURL_NoShellMetacharsLeak(t *testing.T) {
	// A template path is resolved against the conductor base.
	got, err := buildRemoteScriptURL("/t/install-foo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(got, "/t/install-foo") {
		t.Errorf("expected template path suffix, got %q", got)
	}
	if _, err := url.Parse(got); err != nil {
		t.Errorf("built URL must parse: %v", err)
	}

	// A bare token routes to the installer /scripts endpoint with t/i query.
	got, err = buildRemoteScriptURL("install-foo", map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("built URL must parse: %v", err)
	}
	if u.Path != "/scripts" {
		t.Errorf("expected /scripts path, got %q", u.Path)
	}
	if u.Query().Get("t") != "install-foo" {
		t.Errorf("expected t=install-foo, got %q", u.Query().Get("t"))
	}
	if u.Query().Get("i") == "" {
		t.Error("expected non-empty i (base64 params) query")
	}
}

func TestRunRemoteScriptTriggersImmediatePatchFacts(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "script-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}

	oldFetch := fetchRemoteInstructionScript
	oldRun := runRemoteInstructionScript
	fetchRemoteInstructionScript = func(string) (string, error) { return tmp.Name(), nil }
	runRemoteInstructionScript = func(string, time.Duration) scriptRun {
		return scriptRun{Stdout: "ok", ExitCode: 0}
	}
	t.Cleanup(func() {
		fetchRemoteInstructionScript = oldFetch
		runRemoteInstructionScript = oldRun
	})

	collected := make(chan patchfacts.Options, 1)
	reporter := newPatchFactsReporter(
		patchFactsCollectorFunc(func(opts patchfacts.Options) (patchfacts.Record, error) {
			collected <- opts
			return patchfacts.Record{}, nil
		}),
		func(context.Context, patchfacts.Record) patchFactsSendResult {
			return patchFactsSendResult{kind: patchFactsAccepted}
		},
		func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	)
	setActivePatchFactsReporter(reporter)
	t.Cleanup(func() { clearActivePatchFactsReporter(reporter) })

	payload, err := json.Marshal(runRemoteScriptRequest{Script: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := HandleRunRemoteScript(&l2sec.ToNodeAgent{JsonB64: base64.StdEncoding.EncodeToString(payload)}); err != nil {
		t.Fatalf("HandleRunRemoteScript: %v", err)
	}
	select {
	case opts := <-collected:
		if opts.SlowPass {
			t.Error("the remote script trigger used a slow pass")
		}
	case <-time.After(time.Second):
		t.Fatal("the remote script did not trigger patch facts")
	}
}

// Measured on production, 2026-11-27: every remote script reply waited for the
// patch facts collection (apt-get -s upgrade, dpkg-query, systemctl show), which
// costs 1.5 to 13 seconds per node. A script that sets WireGuard peers in a few
// milliseconds answered seconds later, and the control plane request behind it
// ran into its caller's timeout. The reply must not wait for the collection.
func TestRunRemoteScriptRepliesBeforeThePatchFactsCollectionEnds(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "script-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}

	oldFetch := fetchRemoteInstructionScript
	oldRun := runRemoteInstructionScript
	fetchRemoteInstructionScript = func(string) (string, error) { return tmp.Name(), nil }
	runRemoteInstructionScript = func(string, time.Duration) scriptRun {
		return scriptRun{Stdout: "ok", ExitCode: 0}
	}
	t.Cleanup(func() {
		fetchRemoteInstructionScript = oldFetch
		runRemoteInstructionScript = oldRun
	})

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	collected := make(chan struct{})
	reporter := newPatchFactsReporter(
		patchFactsCollectorFunc(func(patchfacts.Options) (patchfacts.Record, error) {
			started <- struct{}{}
			<-release
			close(collected)
			return patchfacts.Record{}, nil
		}),
		func(context.Context, patchfacts.Record) patchFactsSendResult {
			return patchFactsSendResult{kind: patchFactsAccepted}
		},
		func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	)
	setActivePatchFactsReporter(reporter)
	t.Cleanup(func() { clearActivePatchFactsReporter(reporter) })

	payload, err := json.Marshal(runRemoteScriptRequest{Script: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	replied := make(chan error, 1)
	go func() {
		_, err := HandleRunRemoteScript(&l2sec.ToNodeAgent{JsonB64: base64.StdEncoding.EncodeToString(payload)})
		replied <- err
	}()

	select {
	case err := <-replied:
		if err != nil {
			t.Fatalf("HandleRunRemoteScript: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the reply waited for the patch facts collection")
	}

	// The collection still runs, and the record still reaches the mailbox.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the remote script did not start a patch facts collection")
	}
	close(release)
	select {
	case <-collected:
	case <-time.After(2 * time.Second):
		t.Fatal("the patch facts collection did not end")
	}
	deadline := time.Now().Add(2 * time.Second)
	for reporter.pending() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the collected record did not reach the mailbox")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
