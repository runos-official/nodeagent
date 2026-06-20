package agent

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/runos-official/nodeagent/agentstream"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certificate"
	syncUc "github.com/runos-official/nodeagent/uc/sync"
	"github.com/runos-official/nodeagent/vip"

	"time"

	"github.com/spf13/cobra"

	"github.com/runos-official/nodeagent/backend"
)

// RootCmd represents the agent command
var RootCmd = &cobra.Command{
	Use:   "agent",
	Short: "Start the agent",
	Long:  `Sync this node with Nodeward control plane`,
	Run: func(cmd *cobra.Command, args []string) {
		// Check certificate expiry and auto-renew if needed
		renewed, err := certificate.CheckAndAutoRenew()
		if err != nil {
			roslog.E("Certificate auto-renewal check failed", err)
		} else if renewed {
			roslog.I("Certificate was automatically renewed")
		}

		if err := syncUc.ForceVpnSync(); err != nil {
			roslog.E("Error syncing VPN before agent start, things may not work as expected", err)
		}
		runAgent()
	},
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
