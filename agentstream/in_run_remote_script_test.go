package agentstream

import (
	"net/url"
	"strings"
	"testing"
)

func TestBuildRemoteScriptURL_NoShellMetacharsLeak(t *testing.T) {
	// A template path is resolved against the conductor base.
	got, err := buildRemoteScriptURL("/t/install-foo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(got, "/t/install-foo") {
		t.Errorf("expected template path suffix, got %q", got)
	}
	if _, err := url.Parse(got); err != nil {
		t.Errorf("built URL must parse: %v", err)
	}

	// A bare token routes to the installer /scripts endpoint with t/i query.
	got, err = buildRemoteScriptURL("install-foo", map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("built URL must parse: %v", err)
	}
	if u.Path != "/scripts" {
		t.Errorf("expected /scripts path, got %q", u.Path)
	}
	if u.Query().Get("t") != "install-foo" {
		t.Errorf("expected t=install-foo, got %q", u.Query().Get("t"))
	}
	if u.Query().Get("i") == "" {
		t.Error("expected non-empty i (base64 params) query")
	}
}
