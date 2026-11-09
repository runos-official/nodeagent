package agentstream

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/patchfacts"
	"golang.org/x/sys/unix"
)

const (
	// NodePatchFactsMessageType is the outbound stream type that carries one
	// node patch facts record.
	NodePatchFactsMessageType = "NodePatchFactsToServer"

	// NodePatchFactsInterval is the minimum interval between scheduled reports.
	NodePatchFactsInterval = 15 * time.Minute

	nodePatchFactsReplyTimeout = 10 * time.Second
	patchFactsInstallMarker    = "/run/runos/node-patch-facts.pending"
	patchFactsMarkerPoll       = time.Second
)

type nodePatchFactsReply struct {
	Accepted  *bool  `json:"accepted"`
	Message   string `json:"message"`
	Retryable *bool  `json:"retryable"`
}

type patchFactsSendKind uint8

const (
	patchFactsAccepted patchFactsSendKind = iota
	patchFactsRetry
	patchFactsRejected
)

type patchFactsSendResult struct {
	kind    patchFactsSendKind
	message string
	err     error
}

type patchFactsRoundTrip func(context.Context, *l2sec.FromNodeAgent) (*l2sec.ToNodeAgent, error)

// sendNodePatchFacts sends one record and classifies the carrier reply. The
// carrier accepts the record for forwarding. The reply does not mean that the
// final store has written the record.
func sendNodePatchFacts(ctx context.Context, rec patchfacts.Record, roundTrip patchFactsRoundTrip) patchFactsSendResult {
	encoded, err := commons.JSONB64Encode(rec)
	if err != nil {
		return patchFactsSendResult{kind: patchFactsRejected, message: "payload", err: fmt.Errorf("encode node patch facts: %w", err)}
	}

	replyFrame, err := roundTrip(ctx, &l2sec.FromNodeAgent{
		Type:    NodePatchFactsMessageType,
		JsonB64: encoded,
	})
	if err != nil {
		return patchFactsSendResult{kind: patchFactsRetry, err: err}
	}
	if replyFrame == nil {
		return patchFactsSendResult{kind: patchFactsRetry, err: errors.New("node patch facts reply is empty")}
	}
	var reply nodePatchFactsReply
	if err := commons.JSONB64Decode(replyFrame.JsonB64, &reply); err != nil {
		return patchFactsSendResult{kind: patchFactsRetry, err: fmt.Errorf("decode node patch facts reply: %w", err)}
	}
	if reply.Accepted == nil || reply.Retryable == nil {
		return patchFactsSendResult{kind: patchFactsRetry, err: errors.New("node patch facts reply omits accepted or retryable")}
	}
	switch {
	case *reply.Accepted:
		return patchFactsSendResult{kind: patchFactsAccepted, message: reply.Message}
	case *reply.Retryable:
		return patchFactsSendResult{kind: patchFactsRetry, message: reply.Message}
	default:
		return patchFactsSendResult{kind: patchFactsRejected, message: reply.Message}
	}
}

type patchFactsCollector interface {
	Collect(patchfacts.Options) (patchfacts.Record, error)
}

type patchFactsMailbox struct {
	mu         sync.Mutex
	record     patchfacts.Record
	generation uint64
	hasRecord  bool
}

func newPatchFactsMailbox() *patchFactsMailbox {
	return &patchFactsMailbox{}
}

func (m *patchFactsMailbox) replace(rec patchfacts.Record) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generation++
	m.record = rec
	m.hasRecord = true
	return m.generation
}

func (m *patchFactsMailbox) peek() (patchfacts.Record, uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.record, m.generation, m.hasRecord
}

func (m *patchFactsMailbox) remove(generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasRecord || m.generation != generation {
		return false
	}
	m.hasRecord = false
	return true
}

func (m *patchFactsMailbox) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hasRecord {
		return 1
	}
	return 0
}

type patchFactsReporter struct {
	collector patchFactsCollector
	send      func(context.Context, patchfacts.Record) patchFactsSendResult
	after     func(time.Duration) <-chan time.Time
	mailbox   *patchFactsMailbox
	wake      chan struct{}
	backoff   *commons.Backoff
}

func newPatchFactsReporter(
	collector patchFactsCollector,
	send func(context.Context, patchfacts.Record) patchFactsSendResult,
	after func(time.Duration) <-chan time.Time,
) *patchFactsReporter {
	return &patchFactsReporter{
		collector: collector,
		send:      send,
		after:     after,
		mailbox:   newPatchFactsMailbox(),
		wake:      make(chan struct{}, 1),
		backoff:   commons.NewReconnectBackoff(),
	}
}

// trigger collects a fresh record and replaces the one pending record. The
// collection runs before the wake signal, so the retry loop never sends an old
// record after a newer collection has completed.
func (r *patchFactsReporter) trigger(opts patchfacts.Options) error {
	rec, err := r.collector.Collect(opts)
	if err != nil {
		return err
	}
	r.mailbox.replace(rec)
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return nil
}

func (r *patchFactsReporter) pending() int {
	return r.mailbox.len()
}

func (r *patchFactsReporter) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}

		for {
			rec, generation, ok := r.mailbox.peek()
			if !ok {
				break
			}
			result := r.send(ctx, rec)
			switch result.kind {
			case patchFactsAccepted:
				r.backoff.Reset()
				r.mailbox.remove(generation)
				roslog.I("Node patch facts accepted for forwarding", "collectedAt", rec.CollectedAt)
			case patchFactsRejected:
				r.backoff.Reset()
				r.mailbox.remove(generation)
				roslog.E("Node patch facts rejected", result.err, "message", result.message, "collectedAt", rec.CollectedAt)
			case patchFactsRetry:
				delay := r.backoff.Next()
				roslog.W("Node patch facts send failed; retrying", result.err, "message", result.message, "delay", delay)
				select {
				case <-ctx.Done():
					return
				case <-r.wake:
					// A newer record replaces the failed record and sends now.
				case <-r.after(delay):
				}
			}
		}
	}
}

func runPatchFactsSchedule(ctx context.Context, reporter *patchFactsReporter, first, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-first:
			if err := reporter.trigger(patchfacts.Options{SlowPass: true}); err != nil {
				roslog.E("Could not collect scheduled node patch facts", err)
			}
			first = nil
		case <-ticks:
			if err := reporter.trigger(patchfacts.Options{SlowPass: true}); err != nil {
				roslog.E("Could not collect scheduled node patch facts", err)
			}
		}
	}
}

func runPatchFactsDaemonSchedule(
	ctx context.Context,
	reporter *patchFactsReporter,
	slowTicks, markerTicks <-chan time.Time,
	marker string,
	ownerUID uint32,
) {
	lastMarkerError := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-slowTicks:
			if err := reporter.trigger(patchfacts.Options{SlowPass: true}); err != nil {
				roslog.E("Could not collect scheduled node patch facts", err)
			}
		case <-markerTicks:
			if err := consumePatchFactsInstallTriggers(marker, ownerUID, reporter); err != nil {
				if err.Error() != lastMarkerError {
					roslog.E("Could not consume the node patch facts install trigger", err)
					lastMarkerError = err.Error()
				}
			} else {
				lastMarkerError = ""
			}
		}
	}
}

var activePatchFactsReporter struct {
	sync.RWMutex
	reporter *patchFactsReporter
}

func setActivePatchFactsReporter(reporter *patchFactsReporter) {
	activePatchFactsReporter.Lock()
	activePatchFactsReporter.reporter = reporter
	activePatchFactsReporter.Unlock()
}

func clearActivePatchFactsReporter(reporter *patchFactsReporter) {
	activePatchFactsReporter.Lock()
	if activePatchFactsReporter.reporter == reporter {
		activePatchFactsReporter.reporter = nil
	}
	activePatchFactsReporter.Unlock()
}

// TriggerNodePatchFactsReport requests a fresh immediate report. It is safe to
// call when the daemon reporter is not active.
func TriggerNodePatchFactsReport() {
	activePatchFactsReporter.RLock()
	reporter := activePatchFactsReporter.reporter
	activePatchFactsReporter.RUnlock()
	if reporter == nil {
		return
	}
	if err := reporter.trigger(patchfacts.Options{}); err != nil {
		roslog.E("Could not collect immediate node patch facts", err)
	}
}

// StartNodePatchFactsReporter starts the reporter, the random first pass, and
// the fifteen minute schedule. The returned channel closes with the context.
func StartNodePatchFactsReporter(ctx context.Context) chan struct{} {
	reporter := newPatchFactsReporter(
		patchfacts.New(),
		func(ctx context.Context, rec patchfacts.Record) patchFactsSendResult {
			attemptCtx, cancel := context.WithTimeout(ctx, nodePatchFactsReplyTimeout)
			defer cancel()
			return sendNodePatchFacts(attemptCtx, rec, SendAndWaitForResponse)
		},
		time.After,
	)
	done := make(chan struct{})
	setActivePatchFactsReporter(reporter)

	go func() {
		defer close(done)
		defer clearActivePatchFactsReporter(reporter)

		reportDone := make(chan struct{})
		go func() {
			defer close(reportDone)
			reporter.run(ctx)
		}()

		if err := consumePatchFactsInstallTriggers(patchFactsInstallMarker, 0, reporter); err != nil {
			roslog.E("Could not consume the node patch facts install trigger", err)
		}

		offset := time.Duration(rand.Int64N(int64(NodePatchFactsInterval)))
		firstTimer := time.NewTimer(offset)
		defer firstTimer.Stop()
		select {
		case <-ctx.Done():
			<-reportDone
			return
		case <-firstTimer.C:
			if err := reporter.trigger(patchfacts.Options{SlowPass: true}); err != nil {
				roslog.E("Could not collect scheduled node patch facts", err)
			}
		}
		// Start the recurring interval after the randomized first pass. Starting
		// both timers together could put the first two reports less than fifteen
		// minutes apart.
		ticker := time.NewTicker(NodePatchFactsInterval)
		defer ticker.Stop()
		markerTicker := time.NewTicker(patchFactsMarkerPoll)
		defer markerTicker.Stop()
		runPatchFactsDaemonSchedule(ctx, reporter, ticker.C, markerTicker.C, patchFactsInstallMarker, 0)
		<-reportDone
	}()

	return done
}

func markerOwner(info os.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

func validatePatchFactsMarker(path string, ownerUID uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("node patch facts marker %s is not a regular file", path)
	}
	uid, ok := markerOwner(info)
	if !ok || uid != ownerUID {
		return fmt.Errorf("node patch facts marker %s has owner %d, want %d", path, uid, ownerUID)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("node patch facts marker %s is writable by another user", path)
	}
	if info.Size() != 0 {
		return fmt.Errorf("node patch facts marker %s is not empty", path)
	}
	return nil
}

func ensurePatchFactsMarkerDirectory(path string, ownerUID uint32) error {
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("node patch facts marker directory %s is not a directory", dir)
	}
	uid, ok := markerOwner(info)
	if !ok || uid != ownerUID {
		return fmt.Errorf("node patch facts marker directory %s has owner %d, want %d", dir, uid, ownerUID)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("node patch facts marker directory %s is writable by another user", dir)
	}
	return nil
}

func writePatchFactsInstallTrigger(path string, ownerUID uint32) error {
	if err := ensurePatchFactsMarkerDirectory(path, ownerUID); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err == nil {
		return unix.Close(fd)
	}
	if !errors.Is(err, unix.EEXIST) {
		return err
	}
	// An existing safe marker is the bounded coalescing case.
	return validatePatchFactsMarker(path, ownerUID)
}

func consumePatchFactsInstallTriggers(path string, ownerUID uint32, reporter *patchFactsReporter) error {
	processing := path + ".processing"
	for {
		if _, err := os.Lstat(processing); err == nil {
			if err := validatePatchFactsMarker(processing, ownerUID); err != nil {
				return err
			}
			if err := reporter.trigger(patchfacts.Options{}); err != nil {
				return err
			}
			if err := os.Remove(processing); err != nil {
				return err
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}

		err := movePatchFactsMarkerNoReplace(path, processing)
		switch {
		case err == nil:
			continue
		case errors.Is(err, unix.ENOENT):
			return nil
		case errors.Is(err, unix.EEXIST):
			continue
		default:
			return err
		}
	}
}

func notifyPatchFactsAfterInstall(write func() error, logError func(error)) {
	if err := write(); err != nil {
		logError(err)
	}
}

// MarkNodePatchFactsAfterInstall records a bounded notification for the daemon
// process. The install result never depends on marker or transport success.
func MarkNodePatchFactsAfterInstall() {
	notifyPatchFactsAfterInstall(
		func() error { return writePatchFactsInstallTrigger(patchFactsInstallMarker, 0) },
		func(err error) { roslog.E("Could not record the node patch facts install trigger", err) },
	)
}
