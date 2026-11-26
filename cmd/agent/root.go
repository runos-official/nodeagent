package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/runos-official/nodeagent/agentstream"
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certificate"
	"github.com/runos-official/nodeagent/uc/certrenew"
	"github.com/runos-official/nodeagent/uc/certstore"
	syncUc "github.com/runos-official/nodeagent/uc/sync"

	"time"

	"github.com/spf13/cobra"

	"github.com/runos-official/nodeagent/backend"
)

// agentLockPath is the advisory lock taken for the lifetime of the agent so two
// `runos agent` instances cannot race the same :6446 proxy and wg0 interface.
const agentLockPath = "/run/runos/agent.lock"

// RootCmd represents the agent command
var RootCmd = &cobra.Command{
	Use:   "agent",
	Short: "Start the RunOS node agent daemon",
	Long: `Start the RunOS node agent daemon.

Opens the persistent mTLS instruction stream to the Nodeward control plane and
runs the per-connection services (HAProxy Kubernetes API proxy on :6446 and the
heartbeat), supervising and reconnecting on transient failures. The node must
already be registered (run 'runos register ...' from the console first); the
agent verifies its mTLS certificate before starting.`,
	Example: "  runos agent",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return start(agentLockPath, func() {
			// Restore the wg0 boot ordering override before anything else, so this node's NEXT boot
			// brings the tunnel up without waiting on dnsmasq (commons/wireguard_boot.go). One read
			// on a healthy node; a rewrite plus daemon-reload on one installed before the reset.
			commons.EnsureWg0BootOrder()

			// VPN peer sync now runs inside runConnection on every (re)connect, plus a
			// periodic self-heal ticker (see startVpnResyncTicker). Doing it here as a
			// one-shot ran before the network was routable on a hibernate/wake, failed
			// silently (it only logged), and was never retried, leaving wg0 with no
			// peers until a manual `systemctl restart runos`. runConnection only syncs
			// after the stream connects (network confirmed up), so the wake case now
			// self-heals.
			runAgent()
		})
	},
}

// startSteps are the steps of the start path before run, in order. A test
// wraps them to record the order.
var startSteps = struct {
	lock    func(path string) (*os.File, error)
	recover func()
	check   func() error
}{
	lock:    acquireAgentLock,
	recover: certificate.RecoverAtStart,
	check:   checkRegistered,
}

// start is the start path of `runos agent` up to the first dial. lockPath is
// the agent lock; run starts the agent services. Tests inject both.
//
// An expired certificate does not end the start: an expired pair that
// matches passes the registration check, and the agent enters the expired
// state before its first dial (expired_state.go).
func start(lockPath string, run func()) error {
	// Take an advisory, exclusive flock so a second `runos agent` cannot race
	// this one over :6446 / wg0. Held for the process lifetime; the fd is
	// intentionally leaked (released by the kernel on exit). A non-permission
	// failure to acquire the lock means another agent already holds it.
	lockFile, err := startSteps.lock(lockPath)
	if err != nil {
		return err
	}
	if lockFile != nil {
		defer lockFile.Close()
	}

	// Bring the certificate store to rest BEFORE the registration check: a
	// crash between the key rename and the certificate rename of an adoption
	// leaves a key that does not match the certificate, and only the
	// recovery repairs it. On an unregistered node this changes nothing.
	startSteps.recover()

	// Precondition: this node must be registered, i.e. the mTLS cert/key/CA
	// must exist, be non-empty and parseable. Starting without them would
	// fail every Nodeward dial with an opaque mTLS error and spin forever in
	// the supervised reconnect loop, so fail fast with an actionable remedy.
	if err := startSteps.check(); err != nil {
		return err
	}

	run()
	return nil
}

// checkRegistered verifies this node is registered: the mTLS client certificate,
// private key and CA all exist, are non-empty, and the cert/key load as a valid
// keypair. On any failure it returns an already-reported roslog.Fail with the
// remedy to register. It reads the configured paths without creating a file;
// a missing or empty file is the unregistered case.
func checkRegistered() error {
	certPath, keyPath, caPath := config.MTLSPaths()

	for _, p := range []struct{ kind, path string }{
		{"mTLS certificate", certPath},
		{"mTLS private key", keyPath},
		{"CA certificate", caPath},
	} {
		info, err := os.Stat(p.path)
		if err != nil || info.Size() == 0 {
			return roslog.Fail(
				"Start agent",
				fmt.Sprintf("this node is not registered (%s missing or empty at %s)", p.kind, p.path),
				"run `runos register --token <TOKEN> --aid <ACCOUNT_ID>` from the console, then start the agent",
			)
		}
	}

	// Both cert and key are present and non-empty: confirm they parse as a valid
	// mTLS keypair so we catch a truncated / corrupt registration before dialing.
	if _, err := certstore.LoadKeyPair(certPath, keyPath); err != nil {
		return roslog.Fail(
			"Start agent",
			fmt.Sprintf("this node's mTLS certificate/key is unreadable or corrupt (%v)", err),
			"re-register this node: run `runos register --token <TOKEN> --aid <ACCOUNT_ID>` from the console, then start the agent",
		)
	}

	return nil
}

// acquireAgentLock takes an exclusive, non-blocking advisory lock at
// agentLockPath. It returns the open lock file (whose fd holds the lock for the
// process lifetime) on success. If another agent already holds the lock it
// returns an already-reported roslog.Fail. Inability to create/open the lock
// file (e.g. /run not writable in an unusual environment) is treated as a soft
// failure: it logs a warning and returns (nil, nil) so the agent still starts.
func acquireAgentLock(agentLockPath string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(agentLockPath), 0755); err != nil {
		roslog.W("Could not create agent lock directory; skipping single-instance lock", err, "path", agentLockPath)
		return nil, nil
	}

	f, err := os.OpenFile(agentLockPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		roslog.W("Could not open agent lock file; skipping single-instance lock", err, "path", agentLockPath)
		return nil, nil
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, roslog.Fail(
				"Start agent",
				fmt.Sprintf("another `runos agent` instance is already running (lock held at %s)", agentLockPath),
				"stop the running agent first (`systemctl stop runos-agent` or kill the existing process), then retry",
			)
		}
		// Unexpected lock error: warn but do not block startup.
		roslog.W("Could not acquire agent lock; skipping single-instance lock", err, "path", agentLockPath)
		return nil, nil
	}

	return f, nil
}

// Initialize package configuration
func init() {
	// Initialize the control plane nodes registry
	agentstream.InitControlPlaneNodesRegistry()
}

// Reconnect backoff for the supervised stream loop. A transient
// stream/connection error no longer exits the process (which would trigger a
// full systemd Restart=always + re-run of the one-time VPN/VIP/HAProxy setup);
// instead the per-connection services are torn down and re-established
// in-process after a jittered wait (commons.NewReconnectBackoff: random in
// [0, cap], cap 1 s doubling to 30 s, reset after 60 s up). The jitter keeps a
// nodeward or nodehub restart from reconnecting every agent in the same second.

// vpnResyncInterval is the cadence of the background VPN peer self-heal. The
// per-(re)connect sync in runConnection handles the common case; this slow
// ticker only backstops a peer table that goes stale mid-connection (e.g. a
// membership change we missed, or post-resume WireGuard handshake rejection),
// so it is deliberately infrequent to keep Nodeward load negligible.
const vpnResyncInterval = 60 * time.Second

// startVpnResyncTicker re-runs the VPN peer sync every vpnResyncInterval until
// ctx is cancelled (i.e. for the life of one connection). Failures are logged
// and retried on the next tick; they never tear down the connection. It returns
// immediately, running the loop in a goroutine bound to ctx.
func startVpnResyncTicker(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(vpnResyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// An expired certificate is silent here: the dial makes
				// no connection and the expired reporter owns the line.
				if _, err := vpnSync(); err != nil && !backend.IsCertificateExpired(err) {
					roslog.W("Periodic VPN peer resync failed; will retry on the next tick", err)
				}
			}
		}
	}()
}

// handleSignals reads OS signals until a termination signal arrives, then calls
// cancel. SIGHUP is the reload signal that logrotate sends through systemctl
// reload after it renames the log file. It reopens the log file and keeps the
// agent running, so the connection and any running instruction are not
// affected. A failed reopen is logged and does not end the agent.
func handleSignals(sigs <-chan os.Signal, reopenLog func() error, cancel func()) {
	for sig := range sigs {
		switch sig {
		case syscall.SIGHUP:
			if err := reopenLog(); err != nil {
				roslog.W("Received SIGHUP but could not reopen the log file; keeping the old file", err)
				continue
			}
			roslog.I("Received SIGHUP signal, reopened the log file")
		default:
			roslog.I("Received termination signal, shutting down agent", "signal", sig)
			cancel()
			return
		}
	}
}

// runAgent handles the main agent functionality. It supervises the per-connection
// work: the dial, the bidirectional instruction stream, the VPN peer sync (run on
// every connect plus a periodic self-heal ticker), and the proxy/heartbeat
// services that live for the duration of a single connection. A reconnect re-runs
// all of it, so a node that came up before its network was routable converges its
// wg0 peers as soon as it can reach Nodeward.
func runAgent() {
	roslog.I("Starting RunOS Node Agent")

	// Fleet remediation: ensure the on-disk mTLS private key is root-only (0600).
	// The write path (uc/registernode) now creates it 0600, but an already-
	// registered node never rewrites its key, so an existing world-readable
	// (0644) mtls.key is tightened here on every boot. The cert/CA stay public.
	if keyPath := config.GetPrivateKeyPath(); keyPath != "" {
		if _, err := os.Stat(keyPath); err == nil {
			if err := os.Chmod(keyPath, 0600); err != nil {
				roslog.W("Failed to tighten mTLS key permissions on startup", err, "path", keyPath)
			}
		}
	}

	// Top-level context: cancelled only on signal -> ends the whole agent.
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	// Set up signal handling for graceful shutdown and reload.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go handleSignals(sigChan, roslog.ReopenLogFile, rootCancel)

	runAgentWith(rootCtx, agentDeps{})

	roslog.I("Agent terminated")
}

// agentDeps are the parts of runAgentWith that a test replaces. A zero field
// gets the production default.
type agentDeps struct {
	// connect runs one connection; runConnection by default.
	connect func(ctx context.Context, renewal *certrenew.Loop) (shutdown bool, upFor time.Duration)
	// renewal are the dependencies of the renewal loop. OnExpired is always
	// the expired reporter.
	renewal certrenew.Deps
	// hold is the expired state; nil builds it on the configured paths.
	hold func(r *expiredReporter, b *commons.Backoff) *expiredHold
	// wait is the reconnect wait; waitOrDone by default.
	wait func(context.Context, time.Duration)
}

// runAgentWith runs the renewal loop and the supervised connections until
// rootCtx ends.
func runAgentWith(rootCtx context.Context, d agentDeps) {
	if d.connect == nil {
		d.connect = runConnection
	}
	if d.hold == nil {
		d.hold = newExpiredHold
	}
	if d.wait == nil {
		d.wait = waitOrDone
	}

	// One reporter owns the expired line for the life of this process,
	// with or without a stream.
	reporter := newExpiredReporter(config.GetNID(), nil)

	// One certificate renewal loop for the life of this process. It starts
	// before the first dial and waits for a connected stream before its
	// first request. A reconnect does not start, stop or reset it.
	d.renewal.OnExpired = reporter.Report
	renewal := certrenew.New(d.renewal)
	renewal.Start(rootCtx)

	b := commons.NewReconnectBackoff()
	superviseConnections(rootCtx, b, func(ctx context.Context) (bool, time.Duration) {
		return d.connect(ctx, renewal)
	}, d.wait, d.hold(reporter, b))

	// Give a renewal attempt in progress time to adopt its child.
	if !renewal.Wait(certrenew.ShutdownWait) {
		roslog.W("Certificate renewal attempt still running at shutdown; the certificate store repairs it at the next start", nil)
	}
}

// superviseConnections runs connect until it reports a shutdown. After each
// recoverable end it waits b's next delay. connect returns how long the
// connection stayed up (0 when it never came up); a long enough run resets b.
//
// Before each connect, and after each connect that ended with no shutdown,
// it asks h whether the held certificate has expired. An expired certificate
// enters the expired state (h.hold): no dial, no reconnect line and no
// backoff until a valid pair is on disk. A nil h skips the check.
func superviseConnections(
	rootCtx context.Context,
	b *commons.Backoff,
	connect func(context.Context) (shutdown bool, upFor time.Duration),
	wait func(context.Context, time.Duration),
	h *expiredHold,
) {
	for rootCtx.Err() == nil {
		if leaf, expired := h.expired(); expired {
			if h.hold(rootCtx, leaf) {
				return
			}
			continue
		}
		shutdown, upFor := connect(rootCtx)
		if shutdown {
			return
		}
		// A connection that ended on an expired certificate, or a stream
		// that outlived its certificate, enters the expired state.
		if leaf, expired := h.expired(); expired {
			if h.hold(rootCtx, leaf) {
				return
			}
			continue
		}
		// Recoverable error: back off, then re-dial. The per-connection services,
		// including the VPN peer sync, are re-established by the next connect.
		delay := b.AfterConnection(upFor)
		roslog.W("Connection to Nodeward lost; reconnecting after backoff", nil,
			"backoff", delay.String(), "up_for", upFor.Round(time.Second).String())
		wait(rootCtx, delay)
	}
}

// waitOrDone sleeps for d or until ctx ends.
func waitOrDone(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		roslog.I("Shutdown requested during reconnect backoff")
	case <-t.C:
	}
}

// runConnection establishes a single Nodeward connection and runs the
// per-connection services (instruction stream, HAProxy proxy, heartbeat) until
// one stops or rootCtx is cancelled. The returned bool is true iff rootCtx was
// cancelled (permanent shutdown), false on a recoverable error that should
// trigger a reconnect. upFor is how long the stream stayed up (0 when it never
// came up); the caller resets the reconnect backoff after a long enough run.
// renewal hears when the stream is up and when it ends; it may be nil.
func runConnection(rootCtx context.Context, renewal *certrenew.Loop) (shutdown bool, upFor time.Duration) {
	// Per-connection context: cancelled when this connection ends so all of its
	// services unwind, independent of rootCtx (which spans all connections).
	connCtx, connCancel := context.WithCancel(rootCtx)
	defer connCancel()

	// Establish connection to Nodeward (bounded dial; returns an error instead
	// of blocking forever or panicking on a transient failure).
	c, closeConn, err := dialNodeward()
	if err != nil {
		// An expired certificate is the expired state, not a lost
		// connection: superviseConnections holds and the reporter writes the
		// line.
		if !backend.IsCertificateExpired(err) {
			roslog.E("Error connecting to Nodeward", err)
		}
		return rootCtx.Err() != nil, 0
	}
	defer closeConn()

	stream, err := c.NodeAgentStream(context.Background())
	if err != nil {
		roslog.E("Error establishing stream", err)
		return rootCtx.Err() != nil, 0
	}
	connectedAt := time.Now()
	// The renewal loop waits for a connected stream before a request. The
	// signals change no schedule.
	renewal.NotifyConnected()
	defer renewal.NotifyDisconnected()

	roslog.D("Stream established, starting services")

	// The far end for interactive sessions (goal 31). Installed BEFORE the stream handler starts,
	// so an OPEN_STREAM arriving on the first frame finds a dialer rather than a refusal. Without
	// one installed, OPEN_STREAM is refused with a reason saying so, which is the honest answer
	// for a build that cannot serve terminals.
	agentstream.SetDialer(agentstream.DialServiceWebSocket)

	// Start the instruction stream handler.
	streamDone := agentstream.StartInstructionStreamHandler(connCtx, stream)

	// VPN peer self-heal: re-apply the WireGuard peer table on every (re)connect,
	// before HAProxy starts (its Kubernetes API backends are the control-plane
	// peers reached over wg0). This is the reliable path that fixes the
	// wake-from-hibernate case: a node that boots before its network is routable
	// runs its startup sync too early and fails; here we sync only after the
	// stream has connected (network confirmed up), so wg0 converges instead of
	// sitting with a stale/empty peer table until a manual `systemctl restart
	// runos`. Best-effort: a transient sync failure must not tear down an
	// otherwise-healthy connection, so we log and continue; the periodic resync
	// below and the next reconnect retry it.
	if skipped, err := vpnSync(); err != nil {
		if !backend.IsCertificateExpired(err) {
			roslog.W("VPN peer sync on connect failed; will retry on the periodic resync and next reconnect", err)
		}
	} else if skipped > 0 {
		roslog.W("VPN peer sync on connect applied with some peers skipped", nil, "skipped", skipped)
	}

	// Periodic self-heal for the life of this connection: keeps converging the
	// peer table even without a reconnect event (peers changed while we were
	// briefly unreachable, or a post-resume clock skew rejected the initial
	// WireGuard handshakes until a fresh `wg set`).
	startVpnResyncTicker(connCtx)

	// Start the Kubernetes API proxy server.
	proxyDone := connServices.haproxy(connCtx)

	// Start the heartbeat manager.
	heartbeatDone := connServices.heartbeat(connCtx)

	// Report patch state on its own slow schedule. The reporter never uses the
	// five second heartbeat ticker.
	patchFactsDone := connServices.patchFacts(connCtx)

	lifecycleLog("Agent started")

	roslog.I("All services started, agent running")

	// Wait for any service to finish or for the root context to be cancelled.
	var reason string
	select {
	case <-rootCtx.Done():
		roslog.I("Shutdown requested, stopping services")
		reason = "shutdown"
		shutdown = true
	case <-streamDone:
		roslog.I("Instruction stream stopped")
		reason = "stream_stopped"
	case <-proxyDone:
		roslog.I("Kubernetes API proxy stopped")
		reason = "proxy_stopped"
	case <-heartbeatDone:
		roslog.I("Heartbeat manager stopped")
		reason = "heartbeat_stopped"
	case <-patchFactsDone:
		roslog.I("Node patch facts reporter stopped")
		reason = "patch_facts_stopped"
	}
	// Measure before the teardown wait, which can add up to 30 s.
	upFor = time.Since(connectedAt)

	// Cancel the per-connection context so all services unwind.
	connCancel()

	// Wait for a graceful teardown of this connection's services with a timeout.
	waitConnectionServicesDone(streamDone, proxyDone, heartbeatDone, reason)

	if shutdown {
		lifecycleLog("Agent stopped")
	}

	return shutdown, upFor
}

// lifecycleLog sends one agent lifecycle node log entry. An expired
// certificate is silent: the dial makes no connection and the expired
// reporter owns the line.
func lifecycleLog(msg string) {
	if err := addNodelog(3, "NodeAgentLifecycle", msg); err != nil && !backend.IsCertificateExpired(err) {
		roslog.E("Error adding nodelog", err)
	}
}

// Seams of runConnection. Tests replace them; production uses the real
// dial, sync, node log and services.
var (
	dialNodeward = dialL2Sec
	vpnSync      = syncUc.ForceVpnSyncWithCount
	addNodelog   = backend.AddNodelog
	connServices = struct {
		haproxy    func(context.Context) <-chan struct{}
		heartbeat  func(context.Context) chan struct{}
		patchFacts func(context.Context) chan struct{}
	}{
		haproxy:    agentstream.StartHAProxyServer,
		heartbeat:  agentstream.StartNodeAgentHeartbeatManager,
		patchFacts: agentstream.StartNodePatchFactsReporter,
	}
)

// dialL2Sec dials L2Sec and gives the client and the function that closes
// the connection.
func dialL2Sec() (l2sec.NodewardClient, func(), error) {
	c, _, cancel, conn, err := backend.NodewardL2Sec()
	if err != nil {
		return nil, nil, err
	}
	return c, func() {
		cancel()
		conn.Close()
	}, nil
}

// waitConnectionServicesDone waits for the three per-connection service channels
// to close, logging progress, bounded by a 30s timeout so a stuck service cannot
// hang the reconnect indefinitely.
func waitConnectionServicesDone(streamDone, proxyDone, heartbeatDone <-chan struct{}, reason string) {
	shutdownDone := make(chan struct{})
	status := struct {
		streamDone    bool
		proxyDone     bool
		heartbeatDone bool
		mu            sync.Mutex
	}{}

	go func() {
		<-streamDone
		status.mu.Lock()
		status.streamDone = true
		status.mu.Unlock()

		<-proxyDone
		status.mu.Lock()
		status.proxyDone = true
		status.mu.Unlock()

		<-heartbeatDone
		status.mu.Lock()
		status.heartbeatDone = true
		status.mu.Unlock()

		close(shutdownDone)
	}()

	progressTicker := time.NewTicker(1 * time.Second)
	defer progressTicker.Stop()
	shutdownTimeout := time.After(30 * time.Second)

	for {
		select {
		case <-shutdownDone:
			roslog.I("Connection services stopped", "reason", reason)
			return
		case <-progressTicker.C:
			status.mu.Lock()
			waitingFor := []string{}
			if !status.streamDone {
				waitingFor = append(waitingFor, "stream")
			}
			if !status.proxyDone {
				waitingFor = append(waitingFor, "proxy")
			}
			if !status.heartbeatDone {
				waitingFor = append(waitingFor, "heartbeat")
			}
			status.mu.Unlock()
			if len(waitingFor) > 0 {
				roslog.I("Waiting for services to shut down", "services", waitingFor)
			}
		case <-shutdownTimeout:
			roslog.W("Timed out waiting for connection services to stop", nil, "reason", reason)
			return
		}
	}
}
