package certificate

import (
	"github.com/runos-official/nodeagent/uc/certificate"
	"github.com/spf13/cobra"
)

var renewCmd = &cobra.Command{
	Use:   "renew",
	Short: "Renew the node agent mTLS certificate",
	Long: `Renew the mTLS certificate used by the node agent for secure communication.

This command will:
  1. Connect to Nodeward using your current certificate
  2. Request a new certificate
  3. Test the new certificate
  4. Save it to disk if the test succeeds

After successful renewal, you must restart the agent service within 5 minutes:
  sudo systemctl restart runos.service`,
	Run: func(cmd *cobra.Command, args []string) {
		certificate.RenewCertificate()
	},
}
