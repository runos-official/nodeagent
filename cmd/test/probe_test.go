package test

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// serve starts a loopback gRPC server with the given registrations and
// returns a client connection to it.
func serve(t *testing.T, register func(*grpc.Server)) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	register(s)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// FCR 1226 / 1649: the self-test must pass against a nodeward that serves no
// reflection on its registration port.
func TestNodewardAnswersWithoutReflection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("health serving", func(t *testing.T) {
		conn := serve(t, func(s *grpc.Server) { healthpb.RegisterHealthServer(s, health.NewServer()) })
		if err := nodewardAnswers(ctx, conn); err != nil {
			t.Fatalf("nodeward with health and no reflection: %v", err)
		}
	})
	t.Run("older nodeward without health", func(t *testing.T) {
		// A gRPC server that answers Unimplemented is still a server that
		// answers, so the check passes against an older nodeward too.
		conn := serve(t, func(*grpc.Server) {})
		if err := nodewardAnswers(ctx, conn); err != nil {
			t.Fatalf("gRPC server without health: %v", err)
		}
	})
	t.Run("health not serving", func(t *testing.T) {
		conn := serve(t, func(s *grpc.Server) {
			h := health.NewServer()
			h.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
			healthpb.RegisterHealthServer(s, h)
		})
		if err := nodewardAnswers(ctx, conn); err == nil {
			t.Fatal("a NOT_SERVING nodeward passed")
		}
	})
	t.Run("nothing listening", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := lis.Addr().String()
		_ = lis.Close()
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		short, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := nodewardAnswers(short, conn); err == nil {
			t.Fatal("a closed port passed")
		}
	})
}
