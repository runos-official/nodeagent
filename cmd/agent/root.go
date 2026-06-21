package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/runos-official/nodeagent/agentstream"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certificate"
	syncUc "github.com/runos-official/nodeagent/uc/sync"
	"github.com/runos-official/nodeagent/vip"

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
		// Precondition: this node must be registered, i.e. the mTLS cert/key/CA
		// must exist, be non-empty and parseable. Starting without them would
		// fail every Nodeward dial with an opaque mTLS error and spin forever in
		// the supervised reconnect loop, so fail fast with an actionable remedy.
		if err := checkRegistered(); err != nil {
			return err
		}

		// Take an advisory, exclusive flock so a second `runos agent` cannot race
		// this one over :6446 / wg0. Held for the process lifetime; the fd is
		// intentionally leaked (released by the kernel on exit). A non-permission
		// failure to acquire the lock means another agent already holds it.
		lockFile, err := acquireAgentLock()
		if err != nil {
			return err
		}
		if lockFile != nil {
			defer lockFile.Close()
		}

		// Check certificate expiry and auto-renew if needed. If the certificate
		// has ALREADY expired and renewal failed, do not continue into a
		// guaranteed mTLS handshake failure: fail fast with a remedy.
		renewed, err := certificate.CheckAndAutoRenew()
		if err != nil {
			if expired, expiry := certificateExpired(); expired {
				return roslog.Fail(
					"Start agent",
					fmt.Sprintf("mTLS certificate expired on %s and automatic renewal failed: %v", expiry.Format(time.RFC3339), err),
					"check connectivity to the Nodeward control plane, then run `runos certificate renew`; if it keeps failing, re-register this node from the console",
				)
			}
			roslog.E("Certificate auto-renewal check failed", err)
		} else if renewed {
			roslog.I("Certificate was automatically renewed")
		}

		if err := syncUc.ForceVpnSync(); err != nil {
			roslog.W("VPN sync before agent start failed; peers may be stale until the next sync (run `runos sync vpn` to retry). Continuing.", err)
		}
		runAgent()
		return nil
	},
}

// checkRegistered verifies this node is registered: the mTLS client certificate,
// private key and CA all exist, are non-empty, and the cert/key load as a valid
// keypair. On any failure it returns an already-reported roslog.Fail with the
// remedy to register. config.Get*Path create an empty file if missing, so an
// "empty" file is the unregistered case.
func checkRegistered() error {
	certPath := config.GetPublicKeyPath()
	keyPath := config.GetPrivateKeyPath()
	caPath := config.GetCACertPath()

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
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return roslog.Fail(
			"Start agent",
			fmt.Sprintf("this node's mTLS certificate/key is unreadable or corrupt (%v)", err),
			"re-register this node: run `runos register --token <TOKEN> --aid <ACCOUNT_ID>` from the console, then start the agent",
		)
	}

	return nil
}

// certificateExpired reports whether the on-disk mTLS certificate's NotAfter is
// in the past, returning the expiry time. On a read/parse error it returns
// (false, zero) so callers fall back to the non-fatal path.
func certificateExpired() (bool, time.Time) {
	expiry, err := certificate.GetCertificateExpiration()
	if err != nil {
		return false, time.Time{}
	}
	return time.Now().After(expiry), expiry
}

// acquireAgentLock takes an exclusive, non-blocking advisory lock at
// agentLockPath. It returns the open lock file (whose fd holds the lock for the
// process lifetime) on success. If another agent already holds the lock it
// returns an already-reported roslog.Fail. Inability to create/open the lock
// file (e.g. /run not writable in an unusual environment) is treated as a soft
// failure: it logs a warning and returns (nil, nil) so the agent still starts.
func acquireAgentLock() (*os.File, error) {
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

// Reconnect backoff bounds for the supervised stream loop. A transient
// stream/connection error no longer exits the process (which would trigger a
// full systemd Restart=always + re-run of the one-time VPN/VIP/HAProxy setup);
// instead the per-connection services are torn down and re-established in-process
// after an exponentially backed-off delay.
const (
	reconnectInitialBackoff = 1 * time.Second
	reconnectMaxBackoff     = 60 * time.Second
	reconnectBackoffFactor  = 2
)

// runAgent handles the main agent functionality. The one-time startup work
// (initial VPN install/sync) happens in the cobra Run BEFORE this is called and
// is NOT repeated on reconnect. Here we supervise only the per-connection work:
// the dial, the bidirectional instruction stream, and the proxy/heartbeat
// services that live for the duration of a single connection.
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

	go func() {
		sig := <-sigChan
		switch sig {
		case syscall.SIGHUP:
			roslog.I("Received SIGHUP signal, initiating graceful reload")
		case syscall.SIGINT, syscall.SIGTERM:
			roslog.I("Received termination signal, shutting down agent", "signal", sig)
		}
		rootCancel()
	}()

	// stableConnectionThreshold: a connection that stayed up at least this long
	// is considered healthy, so the backoff resets to its initial value (we don't
	// want a brief outage hours later to inherit a maxed-out backoff).
	const stableConnectionThreshold = 2 * time.Minute

	backoff := reconnectInitialBackoff
	for {
		if rootCtx.Err() != nil {
			break
		}

		// runConnection establishes one connection and runs the per-connection
		// services until any of them stops or the root context is cancelled. It
		// returns true if the root context was cancelled (shut down for good),
		// false if it ended due to a recoverable stream/connection error.
		connStart := time.Now()
		shutdown := runConnection(rootCtx)
		if shutdown {
			break
		}

		// If the connection was stable for a while, reset the backoff.
		if time.Since(connStart) >= stableConnectionThreshold {
			backoff = reconnectInitialBackoff
		}

		// Recoverable error: back off, then re-dial. We do NOT re-run the
		// one-time VPN install/sync here; only the per-connection services are
		// re-established.
		roslog.W("Connection to Nodeward lost; reconnecting after backoff", nil, "backoff", backoff.String())
		select {
		case <-rootCtx.Done():
			roslog.I("Shutdown requested during reconnect backoff")
		case <-time.After(backoff):
		}

		// Exponential backoff, capped.
		backoff *= reconnectBackoffFactor
		if backoff > reconnectMaxBackoff {
			backoff = reconnectMaxBackoff
		}
	}

	roslog.I("Agent terminated")
}

// runConnection establishes a single Nodeward connection and runs the
// per-connection services (instruction stream, HAProxy proxy, heartbeat) until
// one stops or rootCtx is cancelled. The returned bool is true iff rootCtx was
// cancelled (permanent shutdown), false on a recoverable error that should
// trigger a reconnect. On a successful, fully-started connection the reconnect
// backoff is reset by the caller via the return path below.
func runConnection(rootCtx context.Context) (shutdown bool) {
	// Per-connection context: cancelled when this connection ends so all of its
	// services unwind, independent of rootCtx (which spans all connections).
	connCtx, connCancel := context.WithCancel(rootCtx)
	defer connCancel()

	// Establish connection to Nodeward (bounded dial; returns an error instead
	// of blocking forever or panicking on a transient failure).
	c, _, _, conn, err := backend.NodewardL2Sec()
	if err != nil {
		roslog.E("Error connecting to Nodeward", err)
		return rootCtx.Err() != nil
	}
	defer conn.Close()

	stream, err := c.NodeAgentStream(context.Background())
	if err != nil {
		roslog.E("Error establishing stream", err)
		return rootCtx.Err() != nil
	}

	roslog.D("Stream established, starting services")

	// Start the instruction stream handler.
	streamDone := agentstream.StartInstructionStreamHandler(connCtx, stream)

	// VIP startup reconcile: query Nodeward authoritatively and converge wg0
	// before the heartbeat starts. The instruction stream must already be running
	// so the response correlates. On failure we tear down this connection and
	// reconnect rather than exiting the process.
	if isHolder, gen, err := agentstream.QueryVipHolderStatus(); err != nil {
		roslog.E("VIP startup query failed, will reconnect", err)
		connCancel()
		<-streamDone
		return rootCtx.Err() != nil
	} else if _, err := vip.Apply(isHolder, gen, false); err != nil {
		roslog.E("VIP startup reconcile failed, will reconnect", err)
		connCancel()
		<-streamDone
		return rootCtx.Err() != nil
	}

	// Start the Kubernetes API proxy server.
	proxyDone := agentstream.StartHAProxyServer(connCtx)

	// Start the heartbeat manager.
	heartbeatDone := agentstream.StartNodeAgentHeartbeatManager(connCtx)

	if err := backend.AddNodelog(3, "NodeAgentLifecycle", "Agent started"); err != nil {
		roslog.E("Error adding nodelog", err)
	}

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
	}

	// Cancel the per-connection context so all services unwind.
	connCancel()

	// Wait for a graceful teardown of this connection's services with a timeout.
	waitConnectionServicesDone(streamDone, proxyDone, heartbeatDone, reason)

	if shutdown {
		logMsg := "Agent stopped"
		if err := backend.AddNodelog(3, "NodeAgentLifecycle", logMsg); err != nil {
			roslog.E("Error adding nodelog", err)
		}
	}

	return shutdown
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
