package certificate

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certificate"
	"github.com/runos-official/nodeagent/uc/certpolicy"
	"github.com/spf13/cobra"
)

// statusJSON toggles machine-readable JSON output for `runos certificate status --json`.
var statusJSON bool

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the node agent mTLS certificate status",
	Long: `Show the current mTLS certificate's expiry and how many days remain.

A renewal is recommended once the certificate is in the last third of its
life (the same due point the agent's auto-renewal uses; each node gets a
fixed offset so a fleet does not renew at once). Pass --json for a stable,
machine-readable object suitable for scripting.`,
	Example: `  # Human-readable status
  runos certificate status

  # Machine-readable output
  runos certificate status --json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		now := time.Now()
		timing, err := certificate.RenewalTiming(now)
		return writeStatus(cmd.OutOrStdout(), statusJSON, timing, err, now)
	},
}

// certificateStatusJSON is the JSON object of `runos certificate status
// --json`. Expired, Result and NextStep are additions; the other members keep
// their names and types.
type certificateStatusJSON struct {
	Status           string `json:"status"`
	NotAfter         string `json:"notAfter"`
	DaysRemaining    int    `json:"daysRemaining"`
	RenewRecommended bool   `json:"renewRecommended"`
	Expired          bool   `json:"expired"`
	// Result is "expired" or "valid".
	Result   string `json:"result"`
	NextStep string `json:"nextStep,omitempty"`
}

// writeStatus writes the status of the held certificate read at now. An
// expired certificate is a successful read: the exit code stays 0, and the
// output names the expiry and the one remedy text, not the renew command
// (which authenticates with the expired certificate and cannot work).
func writeStatus(out io.Writer, jsonOut bool, timing certpolicy.Timing, err error, now time.Time) error {
	notAfter := timing.NotAfter
	if err != nil {
		if jsonOut {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			_ = enc.Encode(struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}{Status: "error", Error: err.Error()})
			return roslog.AlreadyReported(err)
		}
		return roslog.Fail("Read certificate status", err.Error(),
			"if this node is not registered yet, run `runos register` first")
	}

	expired := !now.Before(notAfter)
	daysRemaining := int(notAfter.Sub(now).Hours() / 24)
	renewRecommended := timing.Due && !expired
	result, nextStep := "valid", ""
	if expired {
		result, nextStep = "expired", certpolicy.ExpiredRemedy(notAfter)
	}

	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(certificateStatusJSON{
			Status:           "ok",
			NotAfter:         notAfter.Format(time.RFC3339),
			DaysRemaining:    daysRemaining,
			RenewRecommended: renewRecommended,
			Expired:          expired,
			Result:           result,
			NextStep:         nextStep,
		}); encErr != nil {
			return fmt.Errorf("encoding certificate status JSON: %w", encErr)
		}
		return nil
	}

	if expired {
		fmt.Fprintf(out, "Certificate EXPIRED on %s\n\n%s\n", notAfter.UTC().Format(time.RFC3339), nextStep)
		return nil
	}
	fmt.Fprintf(out, "Certificate expiry: %s\n", notAfter.Format(time.RFC3339))
	fmt.Fprintf(out, "Days remaining:     %d\n", daysRemaining)
	if renewRecommended {
		fmt.Fprintf(out, "\nRenewal recommended. Run: runos certificate renew\n")
	}
	return nil
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output the certificate status as a JSON object")
}
