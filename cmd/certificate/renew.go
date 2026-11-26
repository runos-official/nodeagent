package certificate

import (
	"github.com/runos-official/nodeagent/uc/certificate"
	"github.com/spf13/cobra"
)

// renewJSON toggles machine-readable JSON output for `runos certificate renew --json`.
var renewJSON bool

var renewCmd = &cobra.Command{
	Use:   "renew",
	Short: "Renew the node agent mTLS certificate",
	Long: `Renew the mTLS certificate used by the node agent for secure communication.

This command will:
  1. Take the renewal lock, so only one renewal runs at a time
  2. Connect to Nodeward using your current certificate
  3. Request a new certificate for a fresh local key
  4. Check the new certificate and adopt it in stages (the current
     certificate stays in place when the new one fails a check)

After a successful renewal, restart the agent service within the grace period:
  sudo systemctl restart runos.service

Pass --json for a stable, machine-readable object suitable for scripting.`,
	Example: `  # Renew the certificate (human-readable progress)
  runos certificate renew

  # Renew and emit a machine-readable result
  runos certificate renew --json`,
	Args: cobra.NoArgs,
	// RunE so a failed renewal exits non-zero; failures are reported via
	// roslog.Fail (stderr) inside RenewCertificate, so just return the error.
	RunE: func(cmd *cobra.Command, args []string) error {
		return certificate.RenewCertificate(renewJSON)
	},
}

func init() {
	renewCmd.Flags().BoolVar(&renewJSON, "json", false, "Output the renewal result as a JSON object")
}
