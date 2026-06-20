package config

import (
	"testing"

	"github.com/spf13/viper"
)

// resetConductorState clears the cached conductor and installer values so each
// GetConductorURL case recomputes from a known installer URL. GetConductorURL
// short-circuits when client.server.conductor is already set, so it must be
// cleared between cases.
func resetConductorState(t *testing.T) {
	t.Helper()
	viper.Set("client.server.conductor", "")
	viper.Set("client.server.installer", "")
	t.Cleanup(func() {
		viper.Set("client.server.conductor", "")
		viper.Set("client.server.installer", "")
	})
}

func TestGetConductorUrl_DerivesFromInstaller(t *testing.T) {
	cases := []struct {
		name      string
		installer string
		want      string
	}{
		{"prod get.runos.com", "https://get.runos.com", "https://api.runos.com"},
		{"dev get.dev.runos.com", "https://get.dev.runos.com", "https://api.dev.runos.com"},
		{"already api passthrough", "https://api.runos.com", "https://api.runos.com"},
		{"no get prefix passthrough", "https://installer.example.com", "https://installer.example.com"},
		{"only first get replaced", "https://get.get.runos.com", "https://api.get.runos.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetConductorState(t)
			viper.Set("client.server.installer", tc.installer)

			got := GetConductorURL()
			if got != tc.want {
				t.Fatalf("GetConductorURL() with installer %q = %q, want %q", tc.installer, got, tc.want)
			}
		})
	}
}

func TestGetConductorUrl_PrefersExplicitConductor(t *testing.T) {
	resetConductorState(t)
	// An explicitly configured conductor wins over derivation from the installer.
	viper.Set("client.server.installer", "https://get.runos.com")
	viper.Set("client.server.conductor", "https://custom-conductor.example.com")

	got := GetConductorURL()
	if got != "https://custom-conductor.example.com" {
		t.Fatalf("GetConductorURL() = %q, want the explicit conductor URL", got)
	}
}

func TestSetNodewardHost_SetsNodeward(t *testing.T) {
	orig := viper.GetString("client.server.nodeward")
	t.Cleanup(func() { viper.Set("client.server.nodeward", orig) })

	SetNodewardHost("nodeward.dev.runos.com")
	if got := GetNodewardHost(); got != "nodeward.dev.runos.com" {
		t.Fatalf("GetNodewardHost() = %q, want %q", got, "nodeward.dev.runos.com")
	}

	// Subsequent calls overwrite the previous value.
	SetNodewardHost("nodeward.runos.com")
	if got := GetNodewardHost(); got != "nodeward.runos.com" {
		t.Fatalf("GetNodewardHost() = %q, want %q", got, "nodeward.runos.com")
	}
}

func TestGetCPDomain_ReadsConfiguredValue(t *testing.T) {
	orig := viper.GetString("client.server.cp.domain")
	t.Cleanup(func() { viper.Set("client.server.cp.domain", orig) })

	viper.Set("client.server.cp.domain", "cp.runos.com")
	if got := GetCPDomain(); got != "cp.runos.com" {
		t.Fatalf("GetCPDomain() = %q, want %q", got, "cp.runos.com")
	}
}
