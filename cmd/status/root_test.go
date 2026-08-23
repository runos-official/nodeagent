package status

import (
	"strings"
	"testing"
)

// `runos status` is the first command an operator runs on a node that will not
// join. Before FCR 148 F16 it called testConnection(), threw the error away and
// printed one fixed sentence, so a node that could not resolve the Nodeward host
// was told to run `runos register`. That command has to resolve the same host, so
// it fails the same way. These tests pin three rules: the dial error reaches the
// operator, a diagnosis that carries its own repair is not overridden by a
// generic one, and the diagnosis is printed once rather than twice.

// dnsDiagnosis stands in for the text backend.explainNodewardDialFailure attaches
// when it proves the node cannot resolve the Nodeward host. It is deliberately
// several sentences long, because its length is what makes double printing a
// defect rather than a cosmetic nit.
const dnsDiagnosis = "connection failed: could not resolve the Nodeward host " +
	`"nodeward.runos.com" (server misbehaving). The resolver at 127.0.0.53:53 is not ` +
	"answering, so this node has no working DNS resolver and the connection error below " +
	"is a symptom and not the cause. Run 'resolvectl status' to see what each link has. " +
	"Give the node's primary link a resolver with 'resolvectl dns <link> <dns-server>', " +
	"then restart the agent with 'sudo systemctl restart runos.service'."

func diagnosedReport() statusReport {
	return statusReport{
		NodewardHost:       "nodeward.runos.com",
		ConnectionError:    dnsDiagnosis,
		connectionRemedied: true,
	}
}

func TestConnectionDetailSurfacesTheDialError(t *testing.T) {
	detail := connectionDetail(diagnosedReport())

	if detail != dnsDiagnosis {
		t.Errorf("the operator must see the dial error, not a fixed sentence\ngot: %s", detail)
	}
	if strings.Contains(detail, "could not establish an mTLS stream") {
		t.Errorf("the fixed sentence must not replace a real diagnosis\ngot: %s", detail)
	}
}

func TestConnectionDetailFallsBackWhenNoErrorWasCaptured(t *testing.T) {
	detail := connectionDetail(statusReport{NodewardHost: "nodeward.runos.com"})

	if !strings.Contains(detail, "could not establish an mTLS stream to nodeward.runos.com") {
		t.Errorf("want the fallback sentence naming the host\ngot: %s", detail)
	}
}

// renderHuman prints connectionDetail, then failForDegraded prints
// connectionSummary immediately after. A summary that repeats the whole
// diagnosis prints the same paragraph twice and buries the failure block.
func TestConnectionSummaryDoesNotRepeatTheDetail(t *testing.T) {
	r := diagnosedReport()
	cause, remedy := connectionSummary(r)

	if cause == connectionDetail(r) {
		t.Error("the summary must not repeat the detail printed just above it")
	}
	if len(cause) >= len(dnsDiagnosis) {
		t.Errorf("the summary cause must be shorter than the detail\ngot %d chars: %s", len(cause), cause)
	}
	if !strings.Contains(cause, "could not resolve nodeward.runos.com") {
		t.Errorf("the summary must still name the fault and the host\ngot: %s", cause)
	}
	if remedy == "" {
		t.Error("want a remedy line, got none")
	}
}

func TestConnectionSummaryDoesNotSendADNSFaultToRegister(t *testing.T) {
	_, remedy := connectionSummary(diagnosedReport())

	if strings.Contains(remedy, "runos register") {
		t.Errorf("`runos register` resolves the same host, so it cannot repair a DNS "+
			"fault; the detail above already names the repair\ngot: %s", remedy)
	}
}

// `runos status` only ever runs on an installed node, so every command it
// prints must run there too. `runos preflight` does not: its blocking
// ports-free check fails while kubelet holds 10250, it tells the operator to
// reboot, and it closes with "nothing has been installed". All of that is false
// on an installed node and none of it touches the connection fault.
func TestConnectionSummaryKeepsTheGenericRemedyForAnUndiagnosedError(t *testing.T) {
	r := statusReport{
		NodewardHost:    "nodeward.runos.com",
		ConnectionError: "connection failed: context deadline exceeded",
	}

	_, remedy := connectionSummary(r)

	if strings.Contains(remedy, "runos preflight") {
		t.Errorf("`runos preflight` is an install-time gate; it blocks on ports-free "+
			"on every installed node\ngot: %s", remedy)
	}
	// This error names no repair of its own, so name the two checks that do run
	// on an installed node and do narrow the fault.
	for _, want := range []string{"sudo runos test", "resolvectl query nodeward.runos.com"} {
		if !strings.Contains(remedy, want) {
			t.Errorf("remedy does not mention %q\ngot: %s", want, remedy)
		}
	}
}

// An unregistered node can have no Nodeward host at all. The remedy must not
// then print `resolvectl query` with no argument, because that command does not
// run: resolvectl exits 1 on a missing operand.
func TestConnectionSummaryOmitsTheQueryWhenNoHostIsConfigured(t *testing.T) {
	r := statusReport{
		ConnectionError: "connection failed: dial tcp :9192: connect: connection refused",
	}

	_, remedy := connectionSummary(r)

	if strings.Contains(remedy, "resolvectl query") {
		t.Errorf("an empty host must not produce a bare `resolvectl query`\ngot: %s", remedy)
	}
	if !strings.Contains(remedy, "sudo runos test") {
		t.Errorf("the check that still runs must remain\ngot: %s", remedy)
	}
}

func TestConnectionSummaryFallsBackWhenNoErrorWasCaptured(t *testing.T) {
	cause, remedy := connectionSummary(statusReport{NodewardHost: "nodeward.runos.com"})

	if !strings.Contains(cause, "could not establish an mTLS stream to nodeward.runos.com") {
		t.Errorf("want the fallback cause naming the host\ngot: %s", cause)
	}
	if !strings.Contains(remedy, "runos register") {
		t.Errorf("want the original remedy when nothing better is known\ngot: %s", remedy)
	}
}

// failForDegraded must report the cause connectionSummary chose, so the exit-code
// path and the terminal block cannot drift apart.
func TestFailForDegradedReportsTheSummaryCause(t *testing.T) {
	r := diagnosedReport()
	cause, _ := connectionSummary(r)

	err := failForDegraded(r)
	if err == nil {
		t.Fatal("a node that is not connected is degraded; want an error")
	}
	if !strings.Contains(err.Error(), cause) {
		t.Errorf("failForDegraded reports a different cause than connectionSummary chose\n"+
			"want it to contain: %s\ngot: %s", cause, err.Error())
	}
}

// The certificate branches are checked before the connection branch, so a node
// with a bad certificate is still told about the certificate. This pins that the
// new connection branch did not jump the queue.
func TestFailForDegradedStillPrefersACertificateFault(t *testing.T) {
	r := diagnosedReport()
	r.Cert.Error = "no such file"
	r.Cert.Path = "/etc/runos/node.crt"

	err := failForDegraded(r)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Errorf("a missing certificate must still be reported first\ngot: %s", err.Error())
	}
}
