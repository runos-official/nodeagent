package certificate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certstore"
)

// RenewalGracePeriod is how long the previously issued certificate keeps working
// after a renewal, giving the operator time to restart the agent service before
// the old certificate stops being accepted. Kept here (rather than as bare prose
// in the success message) so the value the user is told has a single source.
const RenewalGracePeriod = 5 * time.Minute

// RenewResult is the stable, machine-readable shape emitted by `runos
// certificate renew --json` on success. Status is "renewed" or
// "already_renewed" (another process renewed first; no new request was
// sent). NotAfter is the live certificate's expiry
// (RFC3339); RestartRequired is always true because the running agent must be
// restarted to load the new certificate.
type RenewResult struct {
	Status          string `json:"status"`
	NotAfter        string `json:"notAfter"`
	RestartRequired bool   `json:"restartRequired"`
}

// certDir returns the directory the mTLS certificates live in, derived from the
// configured public-key path rather than hardcoded, so a non-default mtls.crt
// location is reported correctly.
func certDir() string {
	return filepath.Dir(configuredPaths().Cert)
}

// RenewCertificate renews the mTLS certificate for the node agent. It returns an
// error (already reported via roslog.Fail, or as a JSON error object in JSON
// mode) on any failure so the cobra RunE exits non-zero. When jsonOut is true,
// human progress lines are suppressed and a single RenewResult / error object is
// written to stdout.
func RenewCertificate(jsonOut bool) error {
	return renewCertificate(jsonOut, defaultDeps("CertificateRenewal", "Certificate renewed successfully"), os.Stdout)
}

func renewCertificate(jsonOut bool, d deps, out io.Writer) error {
	roslog.I("Starting certificate renewal")

	// progress prints a human-readable progress line, suppressed in JSON mode so
	// stdout carries only the final JSON object.
	progress := func(format string, args ...any) {
		if !jsonOut {
			fmt.Fprintf(out, format, args...)
		}
	}

	// fail reports a failure. In JSON mode it emits a single error object to
	// stdout and returns an already-reported error; otherwise it routes through
	// roslog.Fail (canonical block on stderr + durable log).
	fail := func(step, cause, remedy string) error {
		roslog.E(step, errors.New(cause))
		if jsonOut {
			emitJSONError(out, cause)
			return roslog.AlreadyReported(fmt.Errorf("%s: %s", step, cause))
		}
		return roslog.Fail(step, cause, remedy)
	}

	progress("\n╔═══════════════════════════════════════════════╗\n")
	progress("║   RunOS Certificate Renewal                   ║\n")
	progress("╚═══════════════════════════════════════════════╝\n\n")

	d.OnStep = func(step string) {
		switch step {
		case stepLockWait:
			progress("→ Waiting for another certificate renewal to end...\n\n")
		case stepRequest:
			progress("→ Requesting new certificate from Nodeward...\n")
		case stepAdopt:
			progress("  ✓ New certificate received\n\n")
			progress("→ Checking and adopting the new certificate...\n")
		}
	}
	// The serial of the certificate that this command read at its start.
	d.ExpectedSerial, _ = certstore.Open(d.Paths).HeldSerial()

	res, err := renewOnce(context.Background(), d)
	if err != nil {
		var dialErr *dialError
		switch {
		case errors.Is(err, certstore.ErrRenewalInProgress):
			return fail("Renew certificate: take the renewal lock", certstore.ErrRenewalInProgress.Error(),
				"wait for the other renewal to end, then run the command again")
		case errors.As(err, &dialErr):
			return fail("Renew certificate: connect to Nodeward", err.Error(),
				"check connectivity to Nodeward; if this node is not registered yet, run `runos register` first")
		case errors.Is(err, certstore.ErrChildRejected):
			return fail("Renew certificate: check the new certificate",
				fmt.Sprintf("the new certificate failed the local check: %s", certstore.RejectReason(err)),
				"the current certificate is unchanged; run the command again later, and if it fails again check this node's status in the RunOS console")
		case isStoreError(err):
			return fail("Renew certificate: adopt the new certificate", err.Error(),
				fmt.Sprintf("ensure %s is writable and you are running as root, then run the command again", certDir()))
		default:
			return fail("Renew certificate: request new certificate from Nodeward", err.Error(),
				"retry shortly; if it persists, check Nodeward availability and this node's status in the RunOS console")
		}
	}

	if res.Status == StatusAlreadyRenewed {
		progress("  ✓ The certificate was already renewed; no new request was needed\n\n")
	} else {
		progress("  ✓ New certificate adopted\n\n")
	}
	if res.NodeLogFailed {
		progress("  ! Could not record the renewal in the node log; the new certificate stays\n\n")
	}
	roslog.I("Certificate renewal completed", "status", res.Status)

	if jsonOut {
		return emitJSON(out, RenewResult{
			Status:          res.Status,
			NotAfter:        res.NotAfter.Format(time.RFC3339),
			RestartRequired: true,
		})
	}

	// Success message (human mode)
	progress("═══════════════════════════════════════════════\n\n")
	progress("✓ Certificate Renewal Successful!\n\n")
	progress("New certificate valid until: %s\n", res.NotAfter.Format(time.RFC3339))
	progress("New certificates have been written to %s\n\n", certDir())
	progress("Action Required:\n")
	progress("  Please restart the node agent service to load the new certificates:\n\n")
	progress("  sudo systemctl restart runos.service\n\n")
	progress("ℹ  You have a %s grace period. The old certificate remains\n", formatGrace(RenewalGracePeriod))
	progress("   valid during this time.\n\n")

	return nil
}

// isStoreError reports an error of the certificate store (prepare,
// recovery or adoption), as opposed to a refused or failed request.
func isStoreError(err error) bool {
	var se *storeError
	return errors.As(err, &se)
}

// formatGrace renders a renewal grace duration as a short human string (e.g.
// "5-minute") for the success message.
func formatGrace(d time.Duration) string {
	mins := int(d.Minutes())
	if mins > 0 && d == time.Duration(mins)*time.Minute {
		return fmt.Sprintf("%d-minute", mins)
	}
	return d.String()
}

// emitJSON writes a RenewResult to stdout as indented JSON.
func emitJSON(out io.Writer, r RenewResult) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("encoding renew result JSON: %w", err)
	}
	return nil
}

// emitJSONError writes a stable error object to stdout in JSON mode.
func emitJSONError(out io.Writer, message string) {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}{Status: "error", Error: message})
}
