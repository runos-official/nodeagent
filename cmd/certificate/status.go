package certificate

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certificate"
	"github.com/spf13/cobra"
)

// statusJSON toggles machine-readable JSON output for `runos certificate status --json`.
var statusJSON bool

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the node agent mTLS certificate status",
	Long: `Show the current mTLS certificate's expiry and how many days remain.

A renewal is recommended once fewer than ` + fmt.Sprintf("%d", certificate.RenewalThresholdDays) + ` days remain (the same
threshold the agent's auto-renewal uses). Pass --json for a stable,
machine-readable object suitable for scripting.`,
	Example: `  # Human-readable status
  runos certificate status

  # Machine-readable output
  runos certificate status --json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()

		notAfter, err := certificate.GetCertificateExpiration()
		if err != nil {
			if statusJSON {
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

		daysRemaining := int(time.Until(notAfter).Hours() / 24)
		renewRecommended := daysRemaining <= certificate.RenewalThresholdDays

		if statusJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			if encErr := enc.Encode(struct {
				Status           string `json:"status"`
				NotAfter         string `json:"notAfter"`
				DaysRemaining    int    `json:"daysRemaining"`
				RenewRecommended bool   `json:"renewRecommended"`
			}{
				Status:           "ok",
				NotAfter:         notAfter.Format(time.RFC3339),
				DaysRemaining:    daysRemaining,
				RenewRecommended: renewRecommended,
			}); encErr != nil {
				return fmt.Errorf("encoding certificate status JSON: %w", encErr)
			}
			return nil
		}

		fmt.Fprintf(out, "Certificate expiry: %s\n", notAfter.Format(time.RFC3339))
		fmt.Fprintf(out, "Days remaining:     %d\n", daysRemaining)
		if renewRecommended {
			fmt.Fprintf(out, "\nRenewal recommended. Run: runos certificate renew\n")
		}
		return nil
	},
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output the certificate status as a JSON object")
}
