package test

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
)

var RootCmd = &cobra.Command{
	Use:   "test",
	Short: "Tests the agent software",
	Long:  `The command will assert environment compatibility and do sanity checks. Useful during updates.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Add timeout context
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Test connection
		_, _, connCancel, conn := backend.NodewardL1Sec()
		if conn == nil {
			roslog.E("Failed to establish backend connection", nil)
			os.Exit(1)
			return
		}
		defer connCancel()
		defer conn.Close()

		roslog.I("Backend connection established")

		// Create a reflection client
		client := grpc_reflection_v1alpha.NewServerReflectionClient(conn)

		// Send a reflection request with context
		stream, err := client.ServerReflectionInfo(ctx)
		if err != nil {
			roslog.E("Failed to create reflection stream", err)
			os.Exit(1)
			return
		}

		// Request list of services
		if err := stream.Send(&grpc_reflection_v1alpha.ServerReflectionRequest{
			MessageRequest: &grpc_reflection_v1alpha.ServerReflectionRequest_ListServices{
				ListServices: "*",
			},
		}); err != nil {
			roslog.E("Failed to send reflection request", err)
			os.Exit(1)
			return
		}

		// Read the response
		resp, err := stream.Recv()
		if err != nil {
			roslog.E("Failed to receive reflection response", err)
			os.Exit(1)
			return
		}

		servicesResponse := resp.GetListServicesResponse()
		if servicesResponse != nil {
			roslog.I("Found services", "count", len(servicesResponse.Service))
			for _, service := range servicesResponse.Service {
				fmt.Printf("Service: %s\n", service.Name)
			}
		} else {
			roslog.W("No services found", nil)
		}

		// Load CP nodes
		cpNodes, err := backend.GetControlPlaneNodes()
		if err != nil {
			roslog.E("Failed to get control plane nodes", err)
			os.Exit(1)
			return
		}

		roslog.I("Found control plane nodes", "count", len(cpNodes))

		// Send a test log message
		err = backend.AddNodelog(3, "Test", fmt.Sprintf("Test command executed at %s", time.Now().Format(time.RFC3339)))
		if err != nil {
			roslog.E("Failed to add nodelog", err)
			os.Exit(1)
			return
		}

		roslog.I("Nodelog added successfully")

		// If we get here, everything was fine
		fmt.Println("ok")
	},
}
