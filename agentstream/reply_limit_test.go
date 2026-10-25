package agentstream

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/l2sec"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// The limits are pinned together: the agent must stop short of what Nodeward accepts,
// with room for the tag, the type and the protobuf framing around the payload.
func TestReplyLimitStaysBelowTheNodewardLimit(t *testing.T) {
	if NodewardMaxMessageBytes != 16*1024*1024 {
		t.Fatalf("Nodeward limit mirror changed to %d; change it only with Nodeward's L2 limit", NodewardMaxMessageBytes)
	}
	if MaxReplyBytes >= NodewardMaxMessageBytes {
		t.Fatalf("reply limit %d is not below the Nodeward limit %d", MaxReplyBytes, NodewardMaxMessageBytes)
	}
	if room := NodewardMaxMessageBytes - MaxReplyBytes; room < 4096 {
		t.Fatalf("only %d bytes of room for tag, type and framing", room)
	}
}

func replyOfSize(t *testing.T, total int) *l2sec.FromNodeAgent {
	t.Helper()
	msg := &l2sec.FromNodeAgent{Type: "KUBECTL_COMMAND_RESPONSE", Tag: "7f0c9d52-1c3e-4b1a-9a57-0b1d3a6a2e11"}
	msg.JsonB64 = strings.Repeat("A", total-proto.Size(msg)-4)
	// the length prefix of the string grows with its size: fix up exactly
	for proto.Size(msg) < total {
		msg.JsonB64 += "A"
	}
	for proto.Size(msg) > total {
		msg.JsonB64 = msg.JsonB64[:len(msg.JsonB64)-1]
	}
	if got := proto.Size(msg); got != total {
		t.Fatalf("built %d bytes, wanted %d", got, total)
	}
	return msg
}

func TestOversizedReplyBecomesATypedError(t *testing.T) {
	big := replyOfSize(t, 24_086_393) // the size seen on prod (FCR 1288)
	got := boundReply(big)
	if got.Type != "ERROR" {
		t.Fatalf("type = %q, want ERROR (older Nodeward and conductor already understand it)", got.Type)
	}
	if got.Tag != big.Tag {
		t.Fatalf("tag = %q, want %q so the caller is answered", got.Tag, big.Tag)
	}
	for _, want := range []string{ReplyTooLargeCode, "size=24086393", fmt.Sprintf("limit=%d", MaxReplyBytes), "type=KUBECTL_COMMAND_RESPONSE"} {
		if !strings.Contains(got.JsonB64, want) {
			t.Fatalf("error text %q lacks %q", got.JsonB64, want)
		}
	}
	if proto.Size(got) > 1024 {
		t.Fatalf("error reply is %d bytes", proto.Size(got))
	}
}

func TestReplyAtTheLimitIsSentUnchanged(t *testing.T) {
	atLimit := replyOfSize(t, MaxReplyBytes)
	if got := boundReply(atLimit); got != atLimit {
		t.Fatal("a reply at the limit was replaced")
	}
	over := replyOfSize(t, MaxReplyBytes+1)
	if got := boundReply(over); got.Type != "ERROR" {
		t.Fatalf("one byte over the limit: type = %q, want ERROR", got.Type)
	}
	if boundReply(nil) != nil {
		t.Fatal("nil reply (suppressed ack) must stay nil")
	}
}

// nodewardLike accepts messages up to the Nodeward limit, as the real L2 server does.
type nodewardLike struct {
	l2sec.UnimplementedNodewardServer
	script func(l2sec.Nodeward_NodeAgentStreamServer) error
}

func (n *nodewardLike) NodeAgentStream(s l2sec.Nodeward_NodeAgentStreamServer) error {
	return n.script(s)
}

func TestOversizedReplyDoesNotEndTheStream(t *testing.T) {
	var mu sync.Mutex
	var replies []*l2sec.FromNodeAgent
	var recvErr error
	done := make(chan struct{})
	srv := grpc.NewServer(grpc.MaxRecvMsgSize(NodewardMaxMessageBytes), grpc.MaxSendMsgSize(NodewardMaxMessageBytes))
	l2sec.RegisterNodewardServer(srv, &nodewardLike{script: func(s l2sec.Nodeward_NodeAgentStreamServer) error {
		defer close(done)
		for _, tag := range []string{"big", "small"} {
			if err := s.Send(&l2sec.ToNodeAgent{Type: "RUN_KUBECTL_COMMAND", Tag: tag}); err != nil {
				return err
			}
		}
		for i := 0; i < 2; i++ {
			m, err := s.Recv()
			if err != nil {
				mu.Lock()
				recvErr = err
				mu.Unlock()
				return nil
			}
			mu.Lock()
			replies = append(replies, m)
			mu.Unlock()
		}
		return nil
	}})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := l2sec.NewNodewardClient(conn).NodeAgentStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	handlerDone := startInstructionStreamHandler(ctx, stream, func(in *l2sec.ToNodeAgent, _ func(*l2sec.FromNodeAgent) error) *l2sec.FromNodeAgent {
		if in.Tag == "big" {
			m := replyOfSize(t, 24_086_393)
			m.Tag = in.Tag
			return m
		}
		return &l2sec.FromNodeAgent{Type: "KUBECTL_COMMAND_RESPONSE", Tag: in.Tag, JsonB64: "ok"}
	})
	_ = handlerDone

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for both replies")
	}
	mu.Lock()
	defer mu.Unlock()
	if recvErr != nil {
		t.Fatalf("the stream ended: %v", recvErr)
	}
	if len(replies) != 2 {
		t.Fatalf("got %d replies, want 2", len(replies))
	}
	byTag := map[string]*l2sec.FromNodeAgent{}
	for _, r := range replies {
		byTag[r.Tag] = r
	}
	if byTag["big"] == nil || byTag["big"].Type != "ERROR" || !strings.Contains(byTag["big"].JsonB64, ReplyTooLargeCode) {
		t.Fatalf("big reply = %+v, want a REPLY_TOO_LARGE error", byTag["big"])
	}
	if byTag["small"] == nil || byTag["small"].JsonB64 != "ok" {
		t.Fatalf("small reply = %+v", byTag["small"])
	}
}

func TestSendersRefuseAnOversizedMessageWithoutWritingIt(t *testing.T) {
	stream := &failingReplyStream{}
	over := replyOfSize(t, MaxReplyBytes+1)
	if err := sendOnStream(context.Background(), stream, over); err == nil {
		t.Fatal("sendOnStream accepted an oversized message")
	}
	setGlobalStream(stream)
	defer setGlobalStream(nil)
	if err := SendToNodeward(over); err == nil {
		t.Fatal("SendToNodeward accepted an oversized message")
	}
	if stream.sends != 0 {
		t.Fatalf("%d oversized messages reached the stream", stream.sends)
	}
}
