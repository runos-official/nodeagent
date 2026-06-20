package agentstream

import (
	"net"
	"testing"
)

func TestValidateScriptParam(t *testing.T) {
	valid := []string{
		"/t/install-foo",
		"/t/nested/path/v1.2.3",
		"install-node",
		"my_script.sh",
		"a",
		"t/foo",
		"/foo.bar-baz",
	}
	for _, s := range valid {
		if err := validateScriptParam(s); err != nil {
			t.Errorf("expected %q to be valid, got error: %v", s, err)
		}
	}

	invalid := []string{
		"",                    // empty
		"foo bar",             // space
		"foo;rm -rf /",        // semicolon
		"foo$(whoami)",        // command substitution
		"foo`id`",             // backticks
		"foo|bash",            // pipe
		"foo&disown",          // ampersand
		"foo\"quote",          // double quote
		"foo'quote",           // single quote
		"http://evil/x",       // colon + scheme chars
		"../../../etc/passwd", // traversal
		"/t/../escape",        // traversal in template path
		"foo\nbar",            // newline
		"foo>out",             // redirect
	}
	for _, s := range invalid {
		if err := validateScriptParam(s); err == nil {
			t.Errorf("expected %q to be rejected, got nil error", s)
		}
	}
}

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"127.0.0.53",
		"::1",
		"169.254.169.254", // metadata
		"169.254.1.1",     // link-local v4
		"fe80::1",         // link-local v6
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if !isBlockedIP(ip) {
			t.Errorf("expected %q to be blocked", s)
		}
	}
	if !isBlockedIP(nil) {
		t.Error("expected nil IP to be blocked")
	}

	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"10.0.0.5",     // RFC1918 is allowed (in-cluster traffic)
		"172.16.0.1",   // RFC1918 allowed
		"192.168.1.10", // RFC1918 allowed
		"2606:4700:4700::1111",
	}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if isBlockedIP(ip) {
			t.Errorf("expected %q to be allowed", s)
		}
	}
}

func TestValidateOutboundURL_SchemeAndHost(t *testing.T) {
	// Scheme rejection (no resolution needed).
	if _, _, err := validateOutboundURL("ftp://example.com/x", false); err == nil {
		t.Error("expected ftp scheme to be rejected")
	}
	if _, _, err := validateOutboundURL("file:///etc/passwd", false); err == nil {
		t.Error("expected file scheme to be rejected")
	}
	// requireHTTPS rejects http.
	if _, _, err := validateOutboundURL("http://example.com/x", true); err == nil {
		t.Error("expected http to be rejected when requireHTTPS=true")
	}
	// Missing host.
	if _, _, err := validateOutboundURL("http:///path", false); err == nil {
		t.Error("expected URL with no host to be rejected")
	}

	// Literal loopback/metadata IPs are rejected without DNS.
	for _, raw := range []string{
		"http://127.0.0.1/x",
		"https://169.254.169.254/latest/meta-data",
		"http://[::1]:8080/x",
		"http://169.254.1.2/x",
	} {
		if _, _, err := validateOutboundURL(raw, false); err == nil {
			t.Errorf("expected %q (internal/metadata literal) to be rejected", raw)
		}
	}

	// Literal public IP passes (no DNS dependency in test).
	if _, ips, err := validateOutboundURL("https://8.8.8.8/x", true); err != nil {
		t.Errorf("expected public literal IP to pass, got %v", err)
	} else if len(ips) != 1 || !ips[0].Equal(net.ParseIP("8.8.8.8")) {
		t.Errorf("expected resolved IP 8.8.8.8, got %v", ips)
	}
}

func TestValidateDnsmasqContents(t *testing.T) {
	// Legit RunOS split-horizon config passes.
	ok := `# RunOS managed dnsmasq config
bind-interfaces
listen-address=10.0.0.1
no-resolv
cache-size=1000
address=/cluster.local/10.96.0.10
server=/internal.example/10.0.0.53
server=8.8.8.8
domain-needed
bogus-priv

--interface=wg0
`
	if err := validateDnsmasqContents(ok); err != nil {
		t.Errorf("expected legit dnsmasq config to pass, got: %v", err)
	}

	denied := []string{
		"conf-file=/tmp/evil.conf",
		"addn-config=/tmp/evil",
		"addn-hosts=/tmp/hosts",
		"servers-file=/tmp/s",
		"dhcp-script=/tmp/run.sh",
		"dhcp-luascript=/tmp/run.lua",
		"resolv-file=/tmp/r",
		"conf-dir=/etc/dnsmasq.d",
		"hostsdir=/tmp/h",
	}
	for _, line := range denied {
		if err := validateDnsmasqContents(line); err == nil {
			t.Errorf("expected dnsmasq directive %q to be rejected", line)
		}
	}

	// Unknown directive not in the allowlist is rejected.
	if err := validateDnsmasqContents("totally-made-up-directive=1"); err == nil {
		t.Error("expected unknown directive to be rejected")
	}
}

func TestValidateHelmName(t *testing.T) {
	valid := []string{"nginx", "cert-manager", "my.chart_v1", "a", "kube-system"}
	for _, n := range valid {
		if err := validateHelmName("chartName", n); err != nil {
			t.Errorf("expected %q to be valid: %v", n, err)
		}
	}
	invalid := []string{
		"",
		"UpperCase",
		"has space",
		"semi;colon",
		"slash/name",
		"dollar$sign",
		"name`tick",
	}
	for _, n := range invalid {
		if err := validateHelmName("chartName", n); err == nil {
			t.Errorf("expected %q to be rejected", n)
		}
	}
}

func TestValidateHelmValuesURL(t *testing.T) {
	// Empty is allowed (no --values flag).
	if err := validateHelmValuesURL(""); err != nil {
		t.Errorf("expected empty valuesUrl to pass, got %v", err)
	}
	// http rejected (must be https).
	if err := validateHelmValuesURL("http://example.com/values.yaml"); err == nil {
		t.Error("expected http valuesUrl to be rejected")
	}
	// internal/metadata literal rejected.
	if err := validateHelmValuesURL("https://169.254.169.254/values.yaml"); err == nil {
		t.Error("expected metadata valuesUrl to be rejected")
	}
	// public https literal passes.
	if err := validateHelmValuesURL("https://1.1.1.1/values.yaml"); err != nil {
		t.Errorf("expected public https valuesUrl to pass, got %v", err)
	}
}

func TestValidateHelmRepoURL(t *testing.T) {
	if err := validateHelmRepoURL(""); err == nil {
		t.Error("expected empty repoUrl to be rejected")
	}
	// http rejected.
	if err := validateHelmRepoURL("http://charts.example.com"); err == nil {
		t.Error("expected http repoUrl to be rejected")
	}
	// https public literal passes.
	if err := validateHelmRepoURL("https://8.8.8.8/charts"); err != nil {
		t.Errorf("expected https public repoUrl to pass, got %v", err)
	}
	// https metadata rejected.
	if err := validateHelmRepoURL("https://169.254.169.254/charts"); err == nil {
		t.Error("expected metadata repoUrl to be rejected")
	}
	// oci public literal passes; oci metadata rejected.
	if err := validateHelmRepoURL("oci://8.8.8.8/charts"); err != nil {
		t.Errorf("expected oci public repoUrl to pass, got %v", err)
	}
	if err := validateHelmRepoURL("oci://169.254.169.254/charts"); err == nil {
		t.Error("expected oci metadata repoUrl to be rejected")
	}
}

func TestValidateInstallHelmChartRequest(t *testing.T) {
	// Full valid request.
	if err := validateInstallHelmChartRequest("https://8.8.8.8/charts", "nginx", "kube-system", ""); err != nil {
		t.Errorf("expected valid helm request to pass, got %v", err)
	}
	// Bad namespace.
	if err := validateInstallHelmChartRequest("https://8.8.8.8/charts", "nginx", "Bad NS", ""); err == nil {
		t.Error("expected bad namespace to be rejected")
	}
	// Bad repo (http).
	if err := validateInstallHelmChartRequest("http://8.8.8.8/charts", "nginx", "kube-system", ""); err == nil {
		t.Error("expected http repo to be rejected")
	}
}
