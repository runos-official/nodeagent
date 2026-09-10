package agentstream

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/l2sec"
	"google.golang.org/protobuf/proto"
)

type failingReplyStream struct {
	l2sec.Nodeward_NodeAgentStreamClient
	mu    sync.Mutex
	sends int
}

func (s *failingReplyStream) Send(*l2sec.FromNodeAgent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends++
	return errors.New("transport failed")
}

func encodeInstructionPayload(t *testing.T, value any) string {
	t.Helper()
	encoded, err := commons.JSONB64Encode(value)
	if err != nil {
		t.Fatalf("encode instruction payload: %v", err)
	}
	return encoded
}

func TestSharedAcknowledgementHandlersReturnIsolatedReplies(t *testing.T) {
	unchangedCleanupCalled := false
	changedCleanupCalled := false
	changedUpdateCalled := false
	changedRestartCalled := false
	uninstallDelay := 0

	tests := []struct {
		name   string
		tag    string
		handle func() (*l2sec.FromNodeAgent, error)
	}{
		{
			name: "apply custom resource",
			tag:  "apply-cr",
			handle: func() (*l2sec.FromNodeAgent, error) {
				instruction := &l2sec.ToNodeAgent{
					JsonB64: encodeInstructionPayload(t, map[string]string{"crB64": "manifest"}),
				}
				return handleApplyCR(instruction, func(value string) error {
					if value != "manifest" {
						t.Fatalf("unexpected custom resource: %q", value)
					}
					return nil
				})
			},
		},
		{
			name: "apply operator",
			tag:  "apply-operator",
			handle: func() (*l2sec.FromNodeAgent, error) {
				instruction := &l2sec.ToNodeAgent{
					JsonB64: encodeInstructionPayload(t, map[string]string{"crdB64": "operator"}),
				}
				return handleApplyOperator(instruction, func(value string) error {
					if value != "operator" {
						t.Fatalf("unexpected operator: %q", value)
					}
					return nil
				})
			},
		},
		{
			name: "delete custom resource",
			tag:  "delete-cr",
			handle: func() (*l2sec.FromNodeAgent, error) {
				instruction := &l2sec.ToNodeAgent{
					JsonB64: encodeInstructionPayload(t, map[string]string{"type": "Widget", "id": "widget-1"}),
				}
				return handleDeleteCR(instruction, func(group, version, resourceType, namespace, id string) error {
					if fmt.Sprint(group, version, resourceType, namespace, id) != "runos.comv1Widgetwidget-1widget-1" {
						t.Fatal("delete handler changed its operation arguments")
					}
					return nil
				})
			},
		},
		{
			name: "remove etcd member",
			tag:  "remove-etcd",
			handle: func() (*l2sec.FromNodeAgent, error) {
				instruction := &l2sec.ToNodeAgent{
					JsonB64: encodeInstructionPayload(t, map[string]string{"node_ip": "192.0.2.10"}),
				}
				return handleRemoveEtcdMember(instruction, func(nodeIP string) error {
					if nodeIP != "192.0.2.10" {
						t.Fatalf("unexpected node address: %q", nodeIP)
					}
					return nil
				})
			},
		},
		{
			name: "unchanged dnsmasq configuration",
			tag:  "dnsmasq-unchanged",
			handle: func() (*l2sec.FromNodeAgent, error) {
				instruction := &l2sec.ToNodeAgent{
					JsonB64: encodeInstructionPayload(t, map[string]string{"fileContents": "server=192.0.2.53"}),
				}
				return handleUpdateDnsmasq(
					instruction,
					func(string) bool { return false },
					func(string) error { t.Fatal("unchanged configuration was written"); return nil },
					func() error { t.Fatal("unchanged configuration restarted dnsmasq"); return nil },
					func() error { t.Fatal("unchanged configuration restored a backup"); return nil },
					func() { unchangedCleanupCalled = true },
				)
			},
		},
		{
			name: "changed dnsmasq configuration",
			tag:  "dnsmasq-changed",
			handle: func() (*l2sec.FromNodeAgent, error) {
				instruction := &l2sec.ToNodeAgent{
					JsonB64: encodeInstructionPayload(t, map[string]string{"fileContents": "server=192.0.2.54"}),
				}
				return handleUpdateDnsmasq(
					instruction,
					func(string) bool { return true },
					func(string) error { changedUpdateCalled = true; return nil },
					func() error { changedRestartCalled = true; return nil },
					func() error { t.Fatal("successful update restored a backup"); return nil },
					func() { changedCleanupCalled = true },
				)
			},
		},
		{
			name: "schedule uninstall",
			tag:  "uninstall",
			handle: func() (*l2sec.FromNodeAgent, error) {
				return handleUninstallNode(func(delay int) error {
					uninstallDelay = delay
					return nil
				})
			},
		},
	}

	responses := make([]*l2sec.FromNodeAgent, len(tests))
	for index, test := range tests {
		response, err := test.handle()
		if err != nil {
			t.Fatalf("%s returned an error: %v", test.name, err)
		}
		if response != NoContentResponse {
			t.Fatalf("%s did not use the acknowledgement template", test.name)
		}
		responses[index] = response
	}
	if unchangedCleanupCalled {
		t.Fatal("unchanged dnsmasq configuration ran cleanup")
	}
	if !changedUpdateCalled || !changedRestartCalled || !changedCleanupCalled {
		t.Fatal("changed dnsmasq configuration did not complete its success branch")
	}
	if uninstallDelay != uninstallStartDelaySeconds {
		t.Fatalf("uninstall delay changed to %d", uninstallDelay)
	}

	start := make(chan struct{})
	finalized := make([]*l2sec.FromNodeAgent, len(tests))
	var wait sync.WaitGroup
	for index, test := range tests {
		wait.Add(1)
		go func(index int, test struct {
			name   string
			tag    string
			handle func() (*l2sec.FromNodeAgent, error)
		}) {
			defer wait.Done()
			<-start
			finalized[index] = finalizeInstructionResponse(
				&l2sec.ToNodeAgent{Tag: test.tag},
				responses[index],
				nil,
			)
		}(index, test)
	}
	close(start)
	wait.Wait()

	for index, test := range tests {
		response := finalized[index]
		if response.Tag != test.tag {
			t.Fatalf("%s used tag %q", test.name, response.Tag)
		}
		if response.Type != "NO_CONTENT" || response.JsonB64 != "e30=" {
			t.Fatalf("%s changed the acknowledgement payload", test.name)
		}
		if response == NoContentResponse {
			t.Fatalf("%s retained the shared response pointer", test.name)
		}
	}
	if NoContentResponse.Tag != "" {
		t.Fatalf("dispatcher mutated the acknowledgement template tag to %q", NoContentResponse.Tag)
	}
}

func TestFinalizeInstructionResponsePreservesCompatibility(t *testing.T) {
	t.Run("opaque error wins over response", func(t *testing.T) {
		response := finalizeInstructionResponse(
			&l2sec.ToNodeAgent{Tag: "error-request"},
			&l2sec.FromNodeAgent{Type: "IGNORED"},
			errors.New("opaque scheduling failure"),
		)
		if response.Type != "ERROR" || response.Tag != "error-request" || response.JsonB64 != "opaque scheduling failure" {
			t.Fatalf("unexpected error response: %+v", response)
		}
	})

	t.Run("nil response suppresses acknowledgement", func(t *testing.T) {
		if response := finalizeInstructionResponse(&l2sec.ToNodeAgent{Tag: "suppressed"}, nil, nil); response != nil {
			t.Fatalf("expected no response, got %+v", response)
		}
	})

	t.Run("typed response retains payload and unknown fields", func(t *testing.T) {
		original := &l2sec.FromNodeAgent{Type: "STATUS", Tag: "handler-tag", JsonB64: "payload"}
		unknown := []byte{0x98, 0x06, 0x01}
		original.ProtoReflect().SetUnknown(unknown)
		response := finalizeInstructionResponse(
			&l2sec.ToNodeAgent{Tag: "request-tag"},
			original,
			nil,
		)
		if response.Type != original.Type || response.JsonB64 != original.JsonB64 || response.Tag != "request-tag" {
			t.Fatalf("typed response changed: %+v", response)
		}
		expected := &l2sec.FromNodeAgent{Type: original.Type, Tag: "request-tag", JsonB64: original.JsonB64}
		expected.ProtoReflect().SetUnknown(unknown)
		if !proto.Equal(response, expected) {
			t.Fatal("typed response fields changed")
		}
		if string(response.ProtoReflect().GetUnknown()) != string(unknown) {
			t.Fatal("protobuf unknown fields changed")
		}
		if original.Tag != "handler-tag" {
			t.Fatalf("original response tag changed to %q", original.Tag)
		}
	})
}

func TestSafeHandleInstructionReturnsCorrelatedPanicError(t *testing.T) {
	instruction := &l2sec.ToNodeAgent{Type: "PANIC_TEST", Tag: "panic-request"}
	response := safeHandleInstructionWith(2, instruction, func(*l2sec.ToNodeAgent) *l2sec.FromNodeAgent {
		panic("handler failed")
	})
	if response == nil || response.Type != "ERROR" || response.Tag != "panic-request" {
		t.Fatalf("panic response lost correlation: %+v", response)
	}
	if response.JsonB64 != "internal error handling instruction: handler failed" {
		t.Fatalf("unexpected panic text: %q", response.JsonB64)
	}

	next := safeHandleInstructionWith(2, instruction, func(*l2sec.ToNodeAgent) *l2sec.FromNodeAgent {
		return &l2sec.FromNodeAgent{Type: "NO_CONTENT"}
	})
	if next == nil || next.Type != "NO_CONTENT" {
		t.Fatalf("worker did not handle the next instruction: %+v", next)
	}
}

func TestSendFailureDoesNotRetryAcknowledgement(t *testing.T) {
	stream := &failingReplyStream{}
	streamMutex.Lock()
	previousStream := globalStream
	globalStream = stream
	streamMutex.Unlock()
	t.Cleanup(func() {
		streamMutex.Lock()
		globalStream = previousStream
		streamMutex.Unlock()
	})

	err := SendToNodeward(&l2sec.FromNodeAgent{Type: "NO_CONTENT", Tag: "one-attempt"})
	if err == nil || err.Error() != "transport failed" {
		t.Fatalf("unexpected send result: %v", err)
	}
	stream.mu.Lock()
	sends := stream.sends
	stream.mu.Unlock()
	if sends != 1 {
		t.Fatalf("send path attempted %d acknowledgements", sends)
	}
}
