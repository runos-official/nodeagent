package agentstream

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/runos-official/nodeagent/l2sec"
)

// FPL315 phase 8: when an instruction payload has "progressTags": true, every
// progress message the agent streams for it carries the tag
// "<instruction tag>/<n>", n counting from 1. Without the field nothing changes:
// a progress message gets a fresh random tag, as today.

func instructionWith(tag, payload string) *l2sec.ToNodeAgent {
	return &l2sec.ToNodeAgent{Tag: tag, Type: "ANY_STREAMING_INSTRUCTION", JsonB64: base64.StdEncoding.EncodeToString([]byte(payload))}
}

func TestProgressTaggerOnlyWhenAsked(t *testing.T) {
	const tag = "0b6c7c1e-0000-4000-8000-000000000001"
	off := map[string]*l2sec.ToNodeAgent{
		"field absent":       instructionWith(tag, `{"targetKubernetesVersion":"1.0.0"}`),
		"field false":        instructionWith(tag, `{"progressTags":false}`),
		"field not a bool":   instructionWith(tag, `{"progressTags":"true"}`),
		"payload not JSON":   instructionWith(tag, `not json`),
		"payload empty":      {Tag: tag},
		"payload not b64":    {Tag: tag, JsonB64: "%%%"},
		"no instruction tag": instructionWith("", `{"progressTags":true}`),
		"nil instruction":    nil,
	}
	for name, in := range off {
		if p := NewProgressTagger(in); p != nil {
			t.Errorf("%s: want no tagger, got one", name)
		}
	}
	if NewProgressTagger(instructionWith(tag, `{"progressTags":true,"isFirstCp":true}`)) == nil {
		t.Fatal(`"progressTags": true must give a tagger`)
	}
}

func TestProgressTagsCountFromOne(t *testing.T) {
	c := newTestBed(t)
	const tag = "0b6c7c1e-0000-4000-8000-000000000002"
	p := NewProgressTagger(instructionWith(tag, `{"progressTags":true}`))
	for i := 0; i < 12; i++ {
		if err := p.Send(&l2sec.FromNodeAgent{Type: "SomeProgress"}); err != nil {
			t.Fatal(err)
		}
	}
	got := c.byType("SomeProgress")
	if len(got) != 12 {
		t.Fatalf("sent %d, want 12", len(got))
	}
	for i, m := range got {
		if want := fmt.Sprintf("%s/%d", tag, i+1); m.Tag != want {
			t.Fatalf("message %d tag = %q, want %q", i+1, m.Tag, want)
		}
	}
}

func TestProgressTagsKeepWireOrderUnderConcurrentSenders(t *testing.T) {
	// kubeadm output is read by two goroutines (stdout and stderr), so two
	// senders can race. The wire order must still be 1, 2, 3, ...
	c := newTestBed(t)
	const tag = "0b6c7c1e-0000-4000-8000-000000000003"
	p := NewProgressTagger(instructionWith(tag, `{"progressTags":true}`))
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = p.Send(&l2sec.FromNodeAgent{Type: "SomeProgress"})
			}
		}()
	}
	wg.Wait()
	got := c.byType("SomeProgress")
	if len(got) != 200 {
		t.Fatalf("sent %d, want 200", len(got))
	}
	for i, m := range got {
		if want := fmt.Sprintf("%s/%d", tag, i+1); m.Tag != want {
			t.Fatalf("wire message %d tag = %q, want %q", i+1, m.Tag, want)
		}
	}
}

func TestNoProgressTaggerSendsAsToday(t *testing.T) {
	c := newTestBed(t)
	var p *ProgressTagger // the instruction did not ask
	if err := p.Send(&l2sec.FromNodeAgent{Type: "SomeProgress"}); err != nil {
		t.Fatal(err)
	}
	got := c.byType("SomeProgress")
	if len(got) != 1 {
		t.Fatalf("sent %d, want 1", len(got))
	}
	if _, err := uuid.Parse(got[0].Tag); err != nil {
		t.Fatalf("tag %q is not the fresh random tag sent today", got[0].Tag)
	}
}

// The upgrade handler is the instruction that streams progress today. A
// payload that does not parse as an upgrade request fails before any command
// runs, which sends exactly one log line through the handler's real path.
func TestUpgradeLogCarriesProgressTagWhenAsked(t *testing.T) {
	const tag = "0b6c7c1e-0000-4000-8000-000000000004"

	c := newTestBed(t)
	resp, _ := HandleUpgradeNodeK8s(instructionWith(tag, `{"progressTags":true,"isFirstCp":"not a bool"}`))
	if resp == nil || resp.Tag != tag {
		t.Fatalf("the reply must keep the instruction tag, got %+v", resp)
	}
	logs := c.byType(UpgradeNodeK8sLogType)
	if len(logs) != 1 || logs[0].Tag != tag+"/1" {
		t.Fatalf("log tags = %v, want [%s/1]", tagsOf(logs), tag)
	}

	c = newTestBed(t)
	HandleUpgradeNodeK8s(instructionWith(tag, `{"isFirstCp":"not a bool"}`))
	logs = c.byType(UpgradeNodeK8sLogType)
	if len(logs) != 1 || strings.HasPrefix(logs[0].Tag, tag) {
		t.Fatalf("without the field the log tag must be a fresh one, got %v", tagsOf(logs))
	}
}

func tagsOf(ms []*l2sec.FromNodeAgent) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Tag)
	}
	return out
}
