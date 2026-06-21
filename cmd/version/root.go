package version

import (
	"encoding/json"
	"fmt"

	"github.com/runos-official/nodeagent/version"
	"github.com/spf13/cobra"
)

// asJSON toggles machine-readable JSON output for `runos version --json`.
var asJSON bool

var RootCmd = &cobra.Command{
	Use:   "version",
	Short: "Get the installed RunOS Node Agent version",
	Long: `Print the installed RunOS Node Agent version.

By default the bare version string is written to stdout. Pass --json for a
stable, machine-readable object suitable for scripting.`,
	Example: `  # Print the version
  runos version

  # Machine-readable output
  runos version --json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()

		if asJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			if err := enc.Encode(struct {
				Version string `json:"version"`
			}{Version: version.Version}); err != nil {
				return fmt.Errorf("encoding version JSON: %w", err)
			}
			return nil
		}

		fmt.Fprintln(out, version.Version)
		return nil
	},
}

func init() {
	RootCmd.Flags().BoolVar(&asJSON, "json", false, "Output the version as a JSON object")
}
