package agent

import (
	"context"
	"crypto/x509"
	"fmt"
	"sync"
	"time"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certpolicy"
	"github.com/runos-official/nodeagent/uc/certrenew"
	"github.com/runos-official/nodeagent/uc/certstore"
)

// Log messages of the expired state.
const (
	// expiredMessage is the one ERROR line of an expired certificate. The
	// reporter owns it: one line at expiry and one each hour.
	expiredMessage = "Node agent certificate expired"
	// validAgainMessage is the INFO line when a valid pair ends the state.
	validAgainMessage = "Node agent certificate is valid again"
)

const (
	// expiredReportEvery is the repeat of the expired line.
	expiredReportEvery = time.Hour
	// expiredCheckEvery is the period of the check for a valid pair in the
	// hold state.
	expiredCheckEvery = 5 * time.Second
)

// expiredReporter is the one process-scope owner of the expired line. A
// caller reports the expired leaf it sees; the reporter writes one line when
// a serial is first reported and one more each hour while callers report
// that serial. A new serial starts a new count. Safe for several goroutines.
type expiredReporter struct {
	nid string
	now func() time.Time

	mu     sync.Mutex
	serial string
	last   time.Time
}

func newExpiredReporter(nid string, now func() time.Time) *expiredReporter {
	if now == nil {
		now = time.Now
	}
	return &expiredReporter{nid: nid, now: now}
}

// Report reports leaf as expired.
func (r *expiredReporter) Report(leaf certrenew.Leaf) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if leaf.Serial == r.serial && !r.last.IsZero() && now.Sub(r.last) < expiredReportEvery {
		return
	}
	r.serial, r.last = leaf.Serial, now
	roslog.E(expiredMessage, nil,
		"expired_at", leaf.NotAfter.UTC().Format(time.RFC3339),
		"serial", leaf.Serial,
		"nid", r.nid,
		"next_step", certpolicy.ExpiredRemedy(leaf.NotAfter))
}

// expiredHold is the expired state of the agent. superviseConnections asks
// it before each connect and after each connect that ended with no shutdown.
// With an expired held leaf the agent holds: no dial, no reconnect line, no
// reconnect backoff and no exit, until a valid matching pair is on disk.
type expiredHold struct {
	// readLeaf reads the held certificate file.
	readLeaf func() (certrenew.Leaf, error)
	// loadPair loads the key pair through the certificate store reader and
	// returns its leaf. A pair whose key does not match does not load.
	loadPair func() (certrenew.Leaf, error)
	now      func() time.Time
	reporter *expiredReporter
	backoff  *commons.Backoff
	every    time.Duration
}

// newExpiredHold gives the production hold on the configured credential
// paths.
func newExpiredHold(reporter *expiredReporter, b *commons.Backoff) *expiredHold {
	return &expiredHold{
		readLeaf: func() (certrenew.Leaf, error) { return certrenew.ReadLeafFile(config.GetPublicKeyPath()) },
		loadPair: func() (certrenew.Leaf, error) {
			cert, key, _ := config.MTLSPaths()
			return loadPairLeaf(cert, key)
		},
		now:      time.Now,
		reporter: reporter,
		backoff:  b,
		every:    expiredCheckEvery,
	}
}

// loadPairLeaf loads the pair at certPath and keyPath and returns its leaf.
func loadPairLeaf(certPath, keyPath string) (certrenew.Leaf, error) {
	pair, err := certstore.LoadKeyPair(certPath, keyPath)
	if err != nil {
		return certrenew.Leaf{}, err
	}
	c := pair.Leaf
	if c == nil {
		if c, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return certrenew.Leaf{}, err
		}
	}
	return certrenew.Leaf{NotBefore: c.NotBefore, NotAfter: c.NotAfter, Serial: fmt.Sprintf("%x", c.SerialNumber)}, nil
}

// expired reports whether the held leaf has expired: now is not before
// NotAfter, the rule of the policy package. A leaf that does not read is not
// the expired state; the connect reports its own error.
func (h *expiredHold) expired() (certrenew.Leaf, bool) {
	if h == nil {
		return certrenew.Leaf{}, false
	}
	leaf, err := h.readLeaf()
	if err != nil {
		return certrenew.Leaf{}, false
	}
	return leaf, !h.now().Before(leaf.NotAfter)
}

// hold runs the expired state for leaf until a valid matching pair loads or
// ctx ends. It reports whether ctx ended. On a valid pair it writes one INFO
// line and resets the reconnect backoff, so the caller connects at once.
func (h *expiredHold) hold(ctx context.Context, leaf certrenew.Leaf) (shutdown bool) {
	h.reporter.Report(leaf)
	t := time.NewTicker(h.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return true
		case <-t.C:
		}
		now := h.now()
		if pair, err := h.loadPair(); err == nil && now.Before(pair.NotAfter) {
			roslog.I(validAgainMessage,
				"serial", pair.Serial,
				"not_after", pair.NotAfter.UTC().Format(time.RFC3339))
			if h.backoff != nil {
				h.backoff.Reset()
			}
			return false
		}
		// Still expired, or a half-copied pair: report the leaf on disk
		// when it is the expired one, else the leaf of the entry.
		if cur, err := h.readLeaf(); err == nil && !now.Before(cur.NotAfter) {
			leaf = cur
		}
		h.reporter.Report(leaf)
	}
}
