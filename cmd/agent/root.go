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

// runAgent handles the main agent functionality
func runAgent() {
	roslog.I("Starting RunOS Node Agent")

	// Create a cancellable background context for the agent
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up signal handling for graceful shutdown and reload
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// Establish connection to Nodeward
	c, _, _, _ := backend.NodewardL2Sec()
	stream, err := c.NodeAgentStream(context.Background())
	if err != nil {
		roslog.E("Error establishing stream", err)
		return
	}

	roslog.D("Stream established, starting services")

	// Start the instruction stream handler
	streamDone := agentstream.StartInstructionStreamHandler(ctx, stream)

	// VIP startup reconcile: query Nodeward authoritatively and converge wg0
	// before the heartbeat starts (first successful heartbeat = "ready"). The
	// instruction stream must already be running so the response correlates.
	if isHolder, gen, err := agentstream.QueryVipHolderStatus(); err != nil {
		roslog.E("VIP startup query failed, aborting agent start", err)
		return
	} else if _, err := vip.Apply(isHolder, gen, false); err != nil {
		roslog.E("VIP startup reconcile failed, aborting agent start", err)
		return
	}

	// Start the Kubernetes API proxy server
	proxyDone := agentstream.StartHAProxyServer(ctx)

	// Start the heartbeat manager
	heartbeatDone := agentstream.StartNodeAgentHeartbeatManager(ctx)

	if err := backend.AddNodelog(3, "NodeAgentLifecycle", "Agent started"); err != nil {
		roslog.E("Error adding nodelog", err)
	}

	roslog.I("All services started, agent running")

	// Variable to track shutdown reason
	var shutdownReason string

	// Wait for any service to finish or signal to arrive
	select {
	case sig := <-sigChan:
		switch sig {
		case syscall.SIGHUP:
			roslog.I("Received SIGHUP signal, initiating graceful reload")
			shutdownReason = "reload"
		case syscall.SIGINT, syscall.SIGTERM:
			roslog.I("Received termination signal, shutting down agent", "signal", sig)
			shutdownReason = "termination"
		}
		cancel() // This will stop all services
	case <-streamDone:
		roslog.I("Instruction stream stopped, shutting down agent")
		shutdownReason = "stream_stopped"
		cancel() // This will stop all other services
	case <-proxyDone:
		roslog.I("Kubernetes API proxy stopped, shutting down agent")
		shutdownReason = "proxy_stopped"
		cancel() // This will stop all other services
	case <-heartbeatDone:
		roslog.I("Heartbeat manager stopped, shutting down agent")
		shutdownReason = "heartbeat_stopped"
		cancel() // This will stop all other services
	}

	// Wait for a graceful shutdown with timeout
	shutdownDone := make(chan struct{})
	shutdownStatus := struct {
		streamDone    bool
		proxyDone     bool
		heartbeatDone bool
		mu            sync.Mutex
	}{}

	go func() {
		// Wait for all services to complete
		select {
		case <-streamDone:
			shutdownStatus.mu.Lock()
			shutdownStatus.streamDone = true
			shutdownStatus.mu.Unlock()
		default:
			<-streamDone
			shutdownStatus.mu.Lock()
			shutdownStatus.streamDone = true
			shutdownStatus.mu.Unlock()
		}
		select {
		case <-proxyDone:
			shutdownStatus.mu.Lock()
			shutdownStatus.proxyDone = true
			shutdownStatus.mu.Unlock()
		default:
			<-proxyDone
			shutdownStatus.mu.Lock()
			shutdownStatus.proxyDone = true
			shutdownStatus.mu.Unlock()
		}
		select {
		case <-heartbeatDone:
			shutdownStatus.mu.Lock()
			shutdownStatus.heartbeatDone = true
			shutdownStatus.mu.Unlock()
		default:
			<-heartbeatDone
			shutdownStatus.mu.Lock()
			shutdownStatus.heartbeatDone = true
			shutdownStatus.mu.Unlock()
		}
		close(shutdownDone)
	}()

	// Start a ticker to log shutdown progress every second
	progressTicker := time.NewTicker(1 * time.Second)
	defer progressTicker.Stop()

	// Add a timeout to prevent indefinite hanging
	shutdownTimeout := time.After(30 * time.Second)

	for {
		select {
		case <-shutdownDone:
			roslog.I("Agent shut down gracefully", "reason", shutdownReason)
			logMsg := "Agent stopped"
			if shutdownReason == "reload" {
				logMsg = "Agent reloading (graceful restart)"
			}
			if err := backend.AddNodelog(3, "NodeAgentLifecycle", logMsg); err != nil {
				roslog.E("Error adding nodelog", err)
			}
			goto shutdown_complete
		case <-progressTicker.C:
			// Log which services are still running
			shutdownStatus.mu.Lock()
			waitingFor := []string{}
			if !shutdownStatus.streamDone {
				waitingFor = append(waitingFor, "stream")
			}
			if !shutdownStatus.proxyDone {
				waitingFor = append(waitingFor, "proxy")
			}
			if !shutdownStatus.heartbeatDone {
				waitingFor = append(waitingFor, "heartbeat")
			}
			shutdownStatus.mu.Unlock()

			if len(waitingFor) > 0 {
				roslog.I("Waiting for services to shut down", "services", waitingFor)
			}
		case <-shutdownTimeout:
			roslog.W("Timed out waiting for graceful shutdown", nil, "reason", shutdownReason)
			if err := backend.AddNodelog(2, "NodeAgentLifecycle", "Agent forcefully stopped"); err != nil {
				roslog.E("Error adding nodelog", err)
			}
			goto shutdown_complete
		}
	}

shutdown_complete:

	roslog.I("Agent terminated", "reason", shutdownReason)
}
