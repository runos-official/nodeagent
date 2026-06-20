package roslog

import (
	"bytes"
	"strings"
	"testing"
)

func TestFail_WritesBlockAndReturnsError(t *testing.T) {
	var buf bytes.Buffer
	orig := stderr
	stderr = &buf
	defer func() { stderr = orig }()

	err := Fail("Register node", "token expired", "copy a fresh join command")
	if err == nil {
		t.Fatal("Fail returned nil error")
	}
	if !strings.Contains(err.Error(), "Register node") || !strings.Contains(err.Error(), "token expired") {
		t.Fatalf("error %q missing step/cause", err.Error())
	}

	out := buf.String()
	for _, want := range []string{"FAILED:", "Register node", "Cause: token expired", "Try:   copy a fresh join command", "Full log:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestFail_OmitsEmptyCauseAndRemedy(t *testing.T) {
	var buf bytes.Buffer
	orig := stderr
	stderr = &buf
	defer func() { stderr = orig }()

	_ = Fail("Some step", "", "")
	out := buf.String()
	if strings.Contains(out, "Cause:") {
		t.Errorf("unexpected Cause line for empty cause:\n%s", out)
	}
	if strings.Contains(out, "Try:") {
		t.Errorf("unexpected Try line for empty remedy:\n%s", out)
	}
}
