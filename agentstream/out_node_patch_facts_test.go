package agentstream

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/uc/patchfacts"
)

func degradedPatchFactsRecord(t *testing.T) patchfacts.Record {
	t.Helper()
	raw, err := os.ReadFile("../uc/patchfacts/testdata/degraded_node_record.json")
	if err != nil {
		t.Fatalf("read shared degraded-node fixture: %v", err)
	}
	var rec patchfacts.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("decode shared degraded-node fixture: %v", err)
	}
	return rec
}

func acceptedPatchFactsReply(t *testing.T) string {
	t.Helper()
	accepted := true
	retryable := false
	encoded, err := commons.JSONB64Encode(nodePatchFactsReply{Accepted: &accepted, Retryable: &retryable})
	if err != nil {
		t.Fatalf("encode reply: %v", err)
	}
	return encoded
}

func TestNodePatchFactsMessageRoundTripsEveryField(t *testing.T) {
	want := degradedPatchFactsRecord(t)
	var sent *l2sec.FromNodeAgent
	send := func(ctx context.Context, msg *l2sec.FromNodeAgent) (*l2sec.ToNodeAgent, error) {
		sent = msg
		return &l2sec.ToNodeAgent{JsonB64: acceptedPatchFactsReply(t)}, nil
	}

	result := sendNodePatchFacts(context.Background(), want, send)
	if result.kind != patchFactsAccepted {
		t.Fatalf("send result = %v, want accepted", result)
	}
	if sent.Type != NodePatchFactsMessageType {
		t.Fatalf("message type = %q, want %q", sent.Type, NodePatchFactsMessageType)
	}

	var got patchfacts.Record
	if err := commons.JSONB64Decode(sent.JsonB64, &got); err != nil {
		t.Fatalf("decode sent record: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip record differs\ngot:  %#v\nwant: %#v", got, want)
	}
	var fields map[string]json.RawMessage
	if err := commons.JSONB64Decode(sent.JsonB64, &fields); err != nil {
		t.Fatalf("decode sent fields: %v", err)
	}
	gotFields := make([]string, 0, len(fields))
	for field := range fields {
		gotFields = append(gotFields, field)
	}
	sort.Strings(gotFields)
	wantFields := []string{
		"automaticUpdateUnitState", "bootTime", "collectedAt", "fieldErrors",
		"heldPackages", "installedKernels", "lastPackageRunAt", "newestInstalledKernel",
		"packageMetadataAgeSeconds", "packageMetadataSource", "rebootRequired",
		"rebootRequiredSince", "rebootTriggerPackages", "runningKernel",
		"securityUpdateCount", "securityUpdatePackages", "securityUpdatesStale",
	}
	if !reflect.DeepEqual(gotFields, wantFields) {
		t.Errorf("message fields = %v, want exactly %v", gotFields, wantFields)
	}
}

func TestNodePatchFactsReplyControlsRetry(t *testing.T) {
	yes := true
	no := false
	tests := []struct {
		name    string
		reply   nodePatchFactsReply
		wireErr error
		want    patchFactsSendKind
	}{
		{name: "accepted", reply: nodePatchFactsReply{Accepted: &yes, Retryable: &no}, want: patchFactsAccepted},
		{name: "queue full", reply: nodePatchFactsReply{Accepted: &no, Message: "queue full", Retryable: &yes}, want: patchFactsRetry},
		{name: "invalid record", reply: nodePatchFactsReply{Accepted: &no, Message: "runningKernel is required", Retryable: &no}, want: patchFactsRejected},
		{name: "send error", wireErr: errors.New("stream closed"), want: patchFactsRetry},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			send := func(ctx context.Context, msg *l2sec.FromNodeAgent) (*l2sec.ToNodeAgent, error) {
				if tc.wireErr != nil {
					return nil, tc.wireErr
				}
				encoded, err := commons.JSONB64Encode(tc.reply)
				if err != nil {
					t.Fatalf("encode reply: %v", err)
				}
				return &l2sec.ToNodeAgent{JsonB64: encoded}, nil
			}
			if got := sendNodePatchFacts(context.Background(), patchfacts.Record{}, send); got.kind != tc.want {
				t.Errorf("result = %v, want kind %v", got, tc.want)
			}
		})
	}

	t.Run("undecodable reply", func(t *testing.T) {
		send := func(ctx context.Context, msg *l2sec.FromNodeAgent) (*l2sec.ToNodeAgent, error) {
			return &l2sec.ToNodeAgent{JsonB64: "not-base64"}, nil
		}
		if got := sendNodePatchFacts(context.Background(), patchfacts.Record{}, send); got.kind != patchFactsRetry {
			t.Errorf("result = %v, want retry", got)
		}
	})

	t.Run("missing contract fields", func(t *testing.T) {
		send := func(ctx context.Context, msg *l2sec.FromNodeAgent) (*l2sec.ToNodeAgent, error) {
			encoded, err := commons.JSONB64Encode(map[string]any{"isHealthy": true})
			if err != nil {
				t.Fatal(err)
			}
			return &l2sec.ToNodeAgent{JsonB64: encoded}, nil
		}
		if got := sendNodePatchFacts(context.Background(), patchfacts.Record{}, send); got.kind != patchFactsRetry {
			t.Errorf("result = %v, want retry", got)
		}
	})

	t.Run("empty reply", func(t *testing.T) {
		send := func(ctx context.Context, msg *l2sec.FromNodeAgent) (*l2sec.ToNodeAgent, error) {
			return nil, nil
		}
		if got := sendNodePatchFacts(context.Background(), patchfacts.Record{}, send); got.kind != patchFactsRetry {
			t.Errorf("result = %v, want retry", got)
		}
	})
}

type patchFactsCollectorFunc func(patchfacts.Options) (patchfacts.Record, error)

func (f patchFactsCollectorFunc) Collect(opts patchfacts.Options) (patchfacts.Record, error) {
	return f(opts)
}

func TestPatchFactsMailboxIsOneSlotAndNewestWins(t *testing.T) {
	box := newPatchFactsMailbox()
	for i := 1; i <= 100; i++ {
		box.replace(patchfacts.Record{RunningKernel: string(rune(i))})
	}
	if box.len() != 1 {
		t.Fatalf("mailbox length = %d, want 1", box.len())
	}
	rec, _, ok := box.peek()
	if !ok || rec.RunningKernel != string(rune(100)) {
		t.Fatalf("mailbox record = %#v, want the newest record", rec)
	}
}

func TestReporterRetriesFailureAndAcceptedReplyStopsRetry(t *testing.T) {
	wakeRetry := make(chan time.Time, 1)
	var mu sync.Mutex
	attempts := 0
	reporter := newPatchFactsReporter(
		patchFactsCollectorFunc(func(opts patchfacts.Options) (patchfacts.Record, error) {
			return patchfacts.Record{RunningKernel: "6.8.0-test"}, nil
		}),
		func(ctx context.Context, rec patchfacts.Record) patchFactsSendResult {
			mu.Lock()
			defer mu.Unlock()
			attempts++
			if attempts == 1 {
				return patchFactsSendResult{kind: patchFactsRetry, err: errors.New("temporary")}
			}
			return patchFactsSendResult{kind: patchFactsAccepted}
		},
		func(time.Duration) <-chan time.Time { return wakeRetry },
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); reporter.run(ctx) }()

	if err := reporter.trigger(patchfacts.Options{}); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	waitForPatchFacts(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempts == 1
	})
	wakeRetry <- time.Now()
	waitForPatchFacts(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempts == 2 && reporter.pending() == 0
	})

	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2; accepted reply must stop retry", attempts)
	}
	mu.Unlock()
	cancel()
	<-done
}

func TestReporterFailureSendsOnlyTheNewestPendingRecord(t *testing.T) {
	firstSendStarted := make(chan struct{})
	releaseFirstSend := make(chan struct{})
	sent := make(chan string, 3)
	reporter := newPatchFactsReporter(
		patchFactsCollectorFunc(func(opts patchfacts.Options) (patchfacts.Record, error) {
			return patchfacts.Record{}, nil
		}),
		func(ctx context.Context, rec patchfacts.Record) patchFactsSendResult {
			sent <- rec.RunningKernel
			if rec.RunningKernel == "first" {
				close(firstSendStarted)
				<-releaseFirstSend
				return patchFactsSendResult{kind: patchFactsRetry, err: errors.New("temporary")}
			}
			return patchFactsSendResult{kind: patchFactsAccepted}
		},
		func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reporter.run(ctx)

	reporter.mailbox.replace(patchfacts.Record{RunningKernel: "first"})
	reporter.wake <- struct{}{}
	<-firstSendStarted
	reporter.mailbox.replace(patchfacts.Record{RunningKernel: "second"})
	reporter.mailbox.replace(patchfacts.Record{RunningKernel: "newest"})
	select {
	case reporter.wake <- struct{}{}:
	default:
	}
	close(releaseFirstSend)

	if got := <-sent; got != "first" {
		t.Fatalf("first sent record = %q", got)
	}
	if got := <-sent; got != "newest" {
		t.Fatalf("record after recovery = %q, want newest", got)
	}
	waitForPatchFacts(t, func() bool { return reporter.pending() == 0 })
	select {
	case extra := <-sent:
		t.Errorf("stale pending record was also sent: %q", extra)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestReporterPermanentRefusalDropsTheRecord(t *testing.T) {
	var attempts atomic.Int32
	reporter := newPatchFactsReporter(
		patchFactsCollectorFunc(func(opts patchfacts.Options) (patchfacts.Record, error) {
			return patchfacts.Record{}, nil
		}),
		func(context.Context, patchfacts.Record) patchFactsSendResult {
			attempts.Add(1)
			return patchFactsSendResult{kind: patchFactsRejected, message: "runningKernel"}
		},
		func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reporter.run(ctx)
	if err := reporter.trigger(patchfacts.Options{}); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	waitForPatchFacts(t, func() bool { return attempts.Load() == 1 && reporter.pending() == 0 })
}

func TestPatchFactsScheduleUsesSlowPassAndRespectsBound(t *testing.T) {
	if NodePatchFactsInterval < 15*time.Minute {
		t.Fatalf("interval = %s, want at least 15m", NodePatchFactsInterval)
	}
	opts := make(chan patchfacts.Options, 2)
	reporter := newPatchFactsReporter(
		patchFactsCollectorFunc(func(got patchfacts.Options) (patchfacts.Record, error) {
			opts <- got
			return patchfacts.Record{}, nil
		}),
		func(context.Context, patchfacts.Record) patchFactsSendResult {
			return patchFactsSendResult{kind: patchFactsAccepted}
		},
		func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	)
	first := make(chan time.Time, 1)
	ticks := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runPatchFactsSchedule(ctx, reporter, first, ticks)

	first <- time.Now()
	if got := <-opts; !got.SlowPass {
		t.Error("the first scheduled collection did not set SlowPass")
	}
	ticks <- time.Now()
	if got := <-opts; !got.SlowPass {
		t.Error("the recurring collection did not set SlowPass")
	}
}

func TestImmediatePatchFactsTriggerDoesNotUseSlowPass(t *testing.T) {
	opts := make(chan patchfacts.Options, 1)
	reporter := newPatchFactsReporter(
		patchFactsCollectorFunc(func(got patchfacts.Options) (patchfacts.Record, error) {
			opts <- got
			return patchfacts.Record{}, nil
		}),
		func(context.Context, patchfacts.Record) patchFactsSendResult {
			return patchFactsSendResult{kind: patchFactsAccepted}
		},
		func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	)
	if err := reporter.trigger(patchfacts.Options{}); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if got := <-opts; got.SlowPass {
		t.Error("an immediate trigger set SlowPass")
	}
}

func TestInstallTriggerCrossesTheProcessBoundaryAndCoalesces(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/node-patch-facts.pending"
	uid := uint32(os.Getuid())
	if err := writePatchFactsInstallTrigger(marker, uid); err != nil {
		t.Fatalf("first install trigger: %v", err)
	}
	if err := writePatchFactsInstallTrigger(marker, uid); err != nil {
		t.Fatalf("coalesced install trigger: %v", err)
	}

	collections := 0
	reporter := newPatchFactsReporter(
		patchFactsCollectorFunc(func(opts patchfacts.Options) (patchfacts.Record, error) {
			collections++
			if opts.SlowPass {
				t.Error("the install trigger refreshed package metadata")
			}
			return patchfacts.Record{}, nil
		}),
		func(context.Context, patchfacts.Record) patchFactsSendResult {
			return patchFactsSendResult{kind: patchFactsAccepted}
		},
		func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	)
	if err := consumePatchFactsInstallTriggers(marker, uid, reporter); err != nil {
		t.Fatalf("consume install trigger: %v", err)
	}
	if collections != 1 {
		t.Errorf("collections = %d, want one coalesced collection", collections)
	}
	if reporter.pending() != 1 {
		t.Errorf("pending records = %d, want 1", reporter.pending())
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Errorf("pending marker remains after ownership transfer: %v", err)
	}
}

func TestInstallTriggerConsumptionDoesNotLoseAConcurrentNotification(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/node-patch-facts.pending"
	uid := uint32(os.Getuid())
	if err := writePatchFactsInstallTrigger(marker, uid); err != nil {
		t.Fatalf("install trigger: %v", err)
	}

	collections := 0
	reporter := newPatchFactsReporter(
		patchFactsCollectorFunc(func(opts patchfacts.Options) (patchfacts.Record, error) {
			collections++
			if collections == 1 {
				if err := writePatchFactsInstallTrigger(marker, uid); err != nil {
					t.Fatalf("concurrent install trigger: %v", err)
				}
			}
			return patchfacts.Record{RunningKernel: string(rune(collections))}, nil
		}),
		func(context.Context, patchfacts.Record) patchFactsSendResult {
			return patchFactsSendResult{kind: patchFactsAccepted}
		},
		func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	)
	if err := consumePatchFactsInstallTriggers(marker, uid, reporter); err != nil {
		t.Fatalf("consume install triggers: %v", err)
	}
	if collections != 2 {
		t.Errorf("collections = %d, want 2", collections)
	}
	rec, _, ok := reporter.mailbox.peek()
	if !ok || rec.RunningKernel != string(rune(2)) {
		t.Errorf("pending record = %#v, want the concurrent notification's record", rec)
	}
}

func TestRunningReporterConsumesAnInstallTrigger(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/node-patch-facts.pending"
	uid := uint32(os.Getuid())
	collections := make(chan patchfacts.Options, 1)
	reporter := newPatchFactsReporter(
		patchFactsCollectorFunc(func(opts patchfacts.Options) (patchfacts.Record, error) {
			collections <- opts
			return patchfacts.Record{}, nil
		}),
		func(context.Context, patchfacts.Record) patchFactsSendResult {
			return patchFactsSendResult{kind: patchFactsAccepted}
		},
		func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	)
	slowTicks := make(chan time.Time)
	markerTicks := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runPatchFactsDaemonSchedule(ctx, reporter, slowTicks, markerTicks, marker, uid)

	if err := writePatchFactsInstallTrigger(marker, uid); err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	markerTicks <- time.Now()
	select {
	case opts := <-collections:
		if opts.SlowPass {
			t.Error("the running-daemon install trigger used a slow pass")
		}
	case <-time.After(time.Second):
		t.Fatal("the running reporter did not consume the install trigger")
	}
}

func TestInstallTriggerRefusesASymbolicLink(t *testing.T) {
	dir := t.TempDir()
	target := dir + "/target"
	marker := dir + "/node-patch-facts.pending"
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, marker); err != nil {
		t.Fatal(err)
	}
	if err := writePatchFactsInstallTrigger(marker, uint32(os.Getuid())); err == nil {
		t.Fatal("writePatchFactsInstallTrigger accepted a symbolic link")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Errorf("symbolic-link target = %q, want unchanged", got)
	}
}

func TestInstallTriggerRefusesUnsafeExistingMarker(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/node-patch-facts.pending"
	if err := os.WriteFile(marker, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(marker, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := writePatchFactsInstallTrigger(marker, uint32(os.Getuid())); err == nil {
		t.Fatal("writePatchFactsInstallTrigger accepted a writable marker")
	}

	info, err := os.Lstat(marker)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != uint32(os.Getuid()) {
		t.Fatalf("test marker uid = %d", stat.Uid)
	}
}

func TestInstallMarkerFailureIsLoggedAndDoesNotEscape(t *testing.T) {
	logged := make(chan error, 1)
	notifyPatchFactsAfterInstall(
		func() error { return errors.New("runtime directory is read-only") },
		func(err error) { logged <- err },
	)
	select {
	case err := <-logged:
		if err == nil || err.Error() != "runtime directory is read-only" {
			t.Errorf("logged error = %v", err)
		}
	default:
		t.Fatal("the marker failure was not logged")
	}
}

func waitForPatchFacts(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the reporter")
		}
		time.Sleep(time.Millisecond)
	}
}
